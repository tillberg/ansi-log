// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package log implements a simple logging package. It defines a type, Logger,
// with methods for formatting output. It also has a predefined 'standard'
// Logger accessible through helper functions Print[f|ln], Fatal[f|ln], and
// Panic[f|ln], which are easier to use than creating a Logger manually.
// That logger writes to standard error and prints the date and time
// of each logged message.
// The Fatal functions call os.Exit(1) after writing the log message.
// The Panic functions call panic after writing the log message.
package log

import (
    "bytes"
    "fmt"
    "io"
    "os"
    "regexp"
    "runtime"
    "strconv"
    "sync"
    "syscall"
    "time"
    "unsafe"
)

// These flags define which text to prefix to each log entry generated by the Logger.
const (
    // Bits or'ed together to control what's printed.
    // There is no control over the order they appear (the order listed
    // here) or the format they present (as described in the comments).
    // The prefix is followed by a colon only when Llongfile or Lshortfile
    // is specified.
    // For example, flags Ldate | Ltime (or LstdFlags) produce,
    //  2009/01/23 01:23:23 message
    // while flags Ldate | Ltime | Lmicroseconds | Llongfile produce,
    //  2009/01/23 01:23:23.123123 /a/b/c/d.go:23: message
    Ldate         = 1 << iota     // the date in the local time zone: 2009/01/23
    Ltime                         // the time in the local time zone: 01:23:23
    Lmicroseconds                 // microsecond resolution: 01:23:23.123123.  assumes Ltime.
    Llongfile                     // full file name and line number: /a/b/c/d.go:23
    Lshortfile                    // final file name element and line number: d.go:23. overrides Llongfile
    LUTC                          // if Ldate or Ltime is set, use UTC rather than the local time zone
    LstdFlags     = Ldate | Ltime // initial values for the standard logger
)

var ansiColorCodes = map[string]int{
    "r":       0,
    "reset":   0,
    "bright":  1,
    "dim":     2,
    "grey":    30,
    "red":     31,
    "green":   32,
    "yellow":  33,
    "blue":    34,
    "magenta": 35,
    "cyan":    36,
    "white":   37,
}

type WriterState struct {
    lastTempBuf []byte
    termWidth int
}

// ensures atomic writes; shared by all Logger instances
var mutex sync.Mutex
var loggers []*Logger
var writers map[io.Writer]*WriterState = make(map[io.Writer]*WriterState)

func getWriterState(writer io.Writer) *WriterState {
    writerState, ok := writers[writer]
    if !ok {
        writerState = &WriterState{}
        writers[writer] = writerState
    }
    return writerState
}

// These facilitate "nullable" bools for some settings
var yes = true
var no = false
func boolPointer(flag bool) *bool {
    if flag { return &yes }
    return &no
}

const ansiCodeResetAll = 0
const ansiCodeHighestIntensity = 2
const ansiCodeResetForecolor = 39

type ActiveAnsiCodes struct {
    intensity int
    forecolor  int
}

func (codes *ActiveAnsiCodes) anyActive() bool {
    return codes.intensity != 0 || codes.forecolor != 0
}

func (codes *ActiveAnsiCodes) add(code int) {
    if code == ansiCodeResetAll {
        codes.intensity = 0
        codes.forecolor = 0
    } else if code <= ansiCodeHighestIntensity {
        codes.intensity = int(code)
    } else if code == ansiCodeResetForecolor {
        codes.forecolor = 0
    } else {
        codes.forecolor = int(code)
    }
}

func (codes *ActiveAnsiCodes) getResetBytes() []byte {
    if codes.intensity != 0 {
        return ansiBytesResetAll
    }
    if codes.forecolor != 0 {
        return ansiBytesResetForecolor
    }
    return bytesEmpty
}

func getActiveAnsiCodes(buf []byte) *ActiveAnsiCodes {
    var ansiActive ActiveAnsiCodes
    for _, groups := range ansiColorRegexp.FindAllSubmatch(buf, -1) {
        code, _ := strconv.ParseInt(string(groups[1]), 10, 32)
        ansiActive.add(int(code))
    }
    return &ansiActive
}

// GetSize returns the dimensions of the given terminal.
func getTermWidth(writer io.Writer) int {
    writerState := getWriterState(writer)
    if writerState.termWidth != 0 {
        return writerState.termWidth
    }
    var fd int
    if writer == os.Stdout {
        fd = syscall.Stdout
    } else {
        // For custom writers, just use the width we get for stderr. This might not be true in some
        // cases (and for those cases, we should add an option to explicitly set width), but it will
        // be true in most cases.
        fd = syscall.Stderr
    }
    var dimensions [4]uint16
    if _, _, err := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&dimensions)), 0, 0, 0); err != 0 {
        // Fall back to a width of 80
        return 80
    }
    return int(dimensions[1])
}

// A Logger represents an active logging object that generates lines of
// output to an io.Writer.  Each logging operation makes a single call to
// the Writer's Write method.  A Logger can be used simultaneously from
// multiple goroutines; it guarantees to serialize access to the Writer.
type Logger struct {
    prefix []byte     // prefix to write at beginning of each line
    flag   int        // properties
    out    io.Writer  // destination for output
    buf    []byte     // for accumulating text to write
    tmp    []byte     // for formatting the current line
    prefixFormatted      []byte
    partialLinesVisible  *bool
    colorEnabled         *bool
    colorTemplateEnabled *bool
    colorRegexp          *regexp.Regexp
    termWidth            int
    callerFile           string
    callerLine           int
    now                  time.Time
}

// New creates a new Logger.   The out variable sets the
// destination to which log data will be written.
// The prefix appears at the beginning of each generated log line.
// The flag argument defines the logging properties.
func New(out io.Writer, prefix string, flag int) *Logger {
    mutex.Lock()
    defer mutex.Unlock()
    var l = &Logger{out: out, prefix: []byte(prefix), flag: flag}
    l.reprocessPrefix()
    loggers = append(loggers, l)
    return l
}

// newStd duplicates some of the work done by New because we can't call
// reprocessPrefix here (as it creates a circular reference back to std)
func newStd() *Logger {
    var l = &Logger{out: os.Stderr, prefix: []byte{}, flag: LstdFlags}
    l.partialLinesVisible = &yes
    l.colorRegexp = regexp.MustCompile("@\\[([\\w,]+?)(:([^)]*?))?\\]")
    l.colorEnabled = &yes
    l.colorTemplateEnabled = &no
    loggers = append(loggers, l)
    return l
}

var std = newStd()

func isTrueDefaulted(flag *bool, fallback *bool) bool {
    if flag != nil {
        return *flag
    }
    return *fallback
}

func (l *Logger) isColorEnabled() bool {
    return isTrueDefaulted(l.colorEnabled, std.colorEnabled)
}

func (l *Logger) isPartialLinesVisible() bool {
    return isTrueDefaulted(l.partialLinesVisible, std.partialLinesVisible)
}

func (l *Logger) getColorTemplateRegexp() *regexp.Regexp {
    if !isTrueDefaulted(l.colorTemplateEnabled, std.colorTemplateEnabled) {
        return nil
    }
    if l.colorRegexp != nil {
        return l.colorRegexp
    }
    return std.colorRegexp
}

// SetOutput sets the output destination for the logger.
func (l *Logger) SetOutput(w io.Writer) {
    mutex.Lock()
    defer mutex.Unlock()
    l.out = w
}

// Cheap integer to fixed-width decimal ASCII.  Give a negative width to avoid zero-padding.
func itoa(buf *[]byte, i int, wid int) {
    // Assemble decimal in reverse order.
    var b [20]byte
    bp := len(b) - 1
    for i >= 10 || wid > 1 {
        wid--
        q := i / 10
        b[bp] = byte('0' + i - q*10)
        bp--
        i = q
    }
    // i < 10
    b[bp] = byte('0' + i)
    *buf = append(*buf, b[bp:]...)
}

func (l *Logger) formatHeader(buf *[]byte) {
    *buf = append(*buf, l.prefixFormatted...)
    if l.flag&(Ldate|Ltime|Lmicroseconds) != 0 {
        if l.flag&Ldate != 0 {
            year, month, day := l.now.Date()
            itoa(buf, year, 4)
            *buf = append(*buf, '/')
            itoa(buf, int(month), 2)
            *buf = append(*buf, '/')
            itoa(buf, day, 2)
            *buf = append(*buf, ' ')
        }
        if l.flag&(Ltime|Lmicroseconds) != 0 {
            hour, min, sec := l.now.Clock()
            itoa(buf, hour, 2)
            *buf = append(*buf, ':')
            itoa(buf, min, 2)
            *buf = append(*buf, ':')
            itoa(buf, sec, 2)
            if l.flag&Lmicroseconds != 0 {
                *buf = append(*buf, '.')
                itoa(buf, l.now.Nanosecond()/1e3, 6)
            }
            *buf = append(*buf, ' ')
        }
    }
    if l.flag&(Lshortfile|Llongfile) != 0 {
        // XXX Is this transformation idempotent?
        if l.flag&Lshortfile != 0 {
            short := l.callerFile
            for i := len(l.callerFile) - 1; i > 0; i-- {
                if l.callerFile[i] == '/' {
                    short = l.callerFile[i+1:]
                    break
                }
            }
            l.callerFile = short
        }
        *buf = append(*buf, l.callerFile...)
        *buf = append(*buf, ':')
        itoa(buf, l.callerLine, -1)
        *buf = append(*buf, ": "...)
    }
}

var bytesEmpty = []byte("")
var bytesCarriageReturn = []byte("\r")
var bytesNewline = []byte("\n")
var bytesSpace = []byte(" ")

func setTempOutput(out io.Writer, buf []byte) {
    writerState := getWriterState(out)
    var lastBuf = writerState.lastTempBuf
    var lastLen = len(lastBuf)
    if len(buf) >= lastLen && bytes.Equal(lastBuf, buf[:lastLen]) {
        out.Write(buf[lastLen:])
    } else {
        out.Write(getActiveAnsiCodes(lastBuf).getResetBytes())
        out.Write(bytesCarriageReturn)
        out.Write(buf)
        // This results in the cursor being too far to the right, but the only case in which this happens is
        // if we're updating the temp output during `writeLine` below, in which case the cursor's column
        // after this operation doesn't matter.
        for i := len(buf); i < lastLen; i++ {
            out.Write(bytesSpace)
        }
    }
    writerState.lastTempBuf = buf
}

func writeLine(out io.Writer, buf []byte) {
    setTempOutput(out, buf)
    out.Write(getActiveAnsiCodes(buf).getResetBytes())
    out.Write(bytesNewline)
    writers[out].lastTempBuf = bytesEmpty
}

var tempLineSep = []byte(" | ")
var tempLineEllipsis = []byte(" ...")
func updateTempOutput(out io.Writer) {
    maxWidth := getTermWidth(out) - 1
    var bufs [][]byte
    for _, logger := range loggers {
        if logger.isPartialLinesVisible() && logger.out == out {
            // Only include this line if it has visible text in it:
            if len(ansiColorRegexp.ReplaceAll(logger.buf, bytesEmpty)) > 0 {
                bufs = append(bufs, logger.getFormattedLine(logger.buf))
            }
        }
    }
    buf := bytes.Join(bufs, tempLineSep)
    if len(buf) > maxWidth {
        buf = append(buf[:maxWidth - len(tempLineEllipsis)], tempLineEllipsis...)
    }
    setTempOutput(out, buf)
}

func ansiEscapeBytes(colorCode int) []byte {
    buf := []byte{}
    buf = append(buf, ansiBytesEscapeStart...)
    buf = append(buf, fmt.Sprintf("%d", colorCode)...)
    buf = append(buf, ansiBytesEscapeEnd...)
    return buf
}

var bytesComma = []byte(",")
var ansiColorRegexp = regexp.MustCompile("\033\\[(\\d+)m")
var ansiBytesEscapeStart = []byte("\033[")
var ansiBytesEscapeEnd = []byte("m")
var ansiBytesResetAll = []byte("\033[0m")
var ansiBytesResetForecolor = []byte("\033[39m")
func (l *Logger) getFormattedLine(line []byte) []byte {
    l.tmp = l.tmp[:0]
    l.formatHeader(&l.tmp)
    codes := getActiveAnsiCodes(l.tmp)
    l.tmp = append(l.tmp, codes.getResetBytes()...)
    l.tmp = append(l.tmp, line...)
    if !l.isColorEnabled() {
        l.tmp = ansiColorRegexp.ReplaceAll(l.tmp, bytesEmpty)
    }
    return l.tmp
}

func (l *Logger) reprocessPrefix() {
    colorTemplateRegexp := l.getColorTemplateRegexp()
    if colorTemplateRegexp != nil {
        l.prefixFormatted = processColorTemplates(colorTemplateRegexp, l.prefix)
    } else {
        l.prefixFormatted = l.prefix
    }
}

func processColorTemplates(colorTemplateRegexp *regexp.Regexp, buf []byte) []byte {
    // We really want ReplaceAllSubmatchFunc, i.e.: https://github.com/golang/go/issues/5690
    // Instead we call FindSubmatch on each match, which means that backtracking may not be
    // used in custom Regexps (matches must also match on themselves without context).
    colorTemplateReplacer := func(token []byte) []byte {
        tmp2 := []byte{}
        groups := colorTemplateRegexp.FindSubmatch(token)
        var ansiActive ActiveAnsiCodes
        for _, codeBytes := range bytes.Split(groups[1], bytesComma) {
            code, ok := ansiColorCodes[string(codeBytes)]
            if !ok {
                // Don't modify the text if we don't recognize any of the codes
                return groups[0]
            }
            ansiActive.add(code)
            tmp2 = append(tmp2, ansiEscapeBytes(code)...)
        }
        if len(groups[2]) > 0 {
            tmp2 = append(tmp2, groups[3]...)
            tmp2 = append(tmp2, ansiActive.getResetBytes()...)
        }
        return tmp2
    }
    return colorTemplateRegexp.ReplaceAllFunc(buf, colorTemplateReplacer)
}

// Output writes the output for a logging event.  The string s contains
// the text to print after the prefix specified by the flags of the
// Logger.  A newline is appended if the last character of s is not
// already a newline.  Calldepth is used to recover the PC and is
// provided for generality, although at the moment on all pre-defined
// paths it will be 2.
func (l *Logger) Output(calldepth int, s string) error {
    l.now = time.Now() // get this early.
    if l.flag&LUTC != 0 {
        l.now = l.now.UTC()
    }
    mutex.Lock()
    defer mutex.Unlock()
    colorTemplateRegexp := l.getColorTemplateRegexp()
    if colorTemplateRegexp != nil {
        l.buf = append(l.buf, processColorTemplates(colorTemplateRegexp, []byte(s))...)
    } else {
        l.buf = append(l.buf, s...)
    }
    var currLine []byte
    for true {
        var index = bytes.IndexByte(l.buf, '\n')
        if index == -1 {
            break
        }
        currLine = l.buf[:index]
        l.buf = l.buf[index+1:] // Is this super-inefficient? i.e. leaking memory?
        if l.flag&(Lshortfile|Llongfile) != 0 {
            // release lock while getting caller info - it's expensive.
            mutex.Unlock()
            var ok bool
            _, l.callerFile, l.callerLine, ok = runtime.Caller(calldepth)
            if !ok {
                l.callerFile = "???"
                l.callerLine = 0
            }
            mutex.Lock()
        }
        ansiActive := getActiveAnsiCodes(currLine)
        writeLine(l.out, l.getFormattedLine(currLine))
        // XXX This is probably inefficient?:
        if ansiActive.intensity != 0 {
            l.buf = append(ansiEscapeBytes(ansiActive.intensity), l.buf...)
        }
        if ansiActive.forecolor != 0 {
            l.buf = append(ansiEscapeBytes(ansiActive.forecolor), l.buf...)
        }
    }
    updateTempOutput(l.out)
    return nil
}

// Printf calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Printf.
func (l *Logger) Printf(format string, v ...interface{}) {
    l.Output(2, fmt.Sprintf(format, v...))
}

// Print calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Print.
func (l *Logger) Print(v ...interface{}) { l.Output(2, fmt.Sprint(v...)) }

// Println calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Println.
func (l *Logger) Println(v ...interface{}) { l.Output(2, fmt.Sprintln(v...)) }

// Fatal is equivalent to l.Print() followed by a call to os.Exit(1).
func (l *Logger) Fatal(v ...interface{}) {
    l.Output(2, fmt.Sprint(v...))
    os.Exit(1)
}

// Fatalf is equivalent to l.Printf() followed by a call to os.Exit(1).
func (l *Logger) Fatalf(format string, v ...interface{}) {
    l.Output(2, fmt.Sprintf(format, v...))
    os.Exit(1)
}

// Fatalln is equivalent to l.Println() followed by a call to os.Exit(1).
func (l *Logger) Fatalln(v ...interface{}) {
    l.Output(2, fmt.Sprintln(v...))
    os.Exit(1)
}

// Panic is equivalent to l.Print() followed by a call to panic().
func (l *Logger) Panic(v ...interface{}) {
    s := fmt.Sprint(v...)
    l.Output(2, s)
    panic(s)
}

// Panicf is equivalent to l.Printf() followed by a call to panic().
func (l *Logger) Panicf(format string, v ...interface{}) {
    s := fmt.Sprintf(format, v...)
    l.Output(2, s)
    panic(s)
}

// Panicln is equivalent to l.Println() followed by a call to panic().
func (l *Logger) Panicln(v ...interface{}) {
    s := fmt.Sprintln(v...)
    l.Output(2, s)
    panic(s)
}

// Flags returns the output flags for the logger.
func (l *Logger) Flags() int {
    mutex.Lock()
    defer mutex.Unlock()
    return l.flag
}

// SetFlags sets the output flags for the logger.
func (l *Logger) SetFlags(flag int) {
    mutex.Lock()
    defer mutex.Unlock()
    l.flag = flag
}

// Prefix returns the output prefix for the logger.
func (l *Logger) Prefix() string {
    mutex.Lock()
    defer mutex.Unlock()
    return string(l.prefix)
}

// SetPrefix sets the output prefix for the logger.
func (l *Logger) SetPrefix(prefix string) {
    mutex.Lock()
    defer mutex.Unlock()
    l.prefix = []byte(prefix)
    l.reprocessPrefix()
}

func (l *Logger) Close() {
    mutex.Lock()
    if len(l.buf) > 0 {
        mutex.Unlock()
        l.Output(2, "\n")
    } else {
        mutex.Unlock()
    }
}



func (l *Logger) SetPartialLinesVisible(flag bool) {
    mutex.Lock()
    defer mutex.Unlock()
    l.partialLinesVisible = boolPointer(flag)
}

func (l *Logger) ShowPartialLines() { l.SetPartialLinesVisible(true) }

func (l *Logger) HidePartialLines() { l.SetPartialLinesVisible(false) }

func (l *Logger) SetColorEnabled(flag bool) {
    mutex.Lock()
    defer mutex.Unlock()
    l.colorEnabled = boolPointer(flag)
}

func (l *Logger) EnableColor() { l.SetColorEnabled(true) }

func (l *Logger) DisableColor() { l.SetColorEnabled(false) }

func (l *Logger) SetColorTemplateEnabled(flag bool) {
    mutex.Lock()
    defer mutex.Unlock()
    l.colorTemplateEnabled = boolPointer(flag)
    l.reprocessPrefix()
}

func (l* Logger) EnableColorTemplate() { l.SetColorTemplateEnabled(true) }
func (l* Logger) DisableColorTemplate() { l.SetColorTemplateEnabled(false) }

func (l *Logger) SetColorTemplateRegexp(rgx *regexp.Regexp) {
    mutex.Lock()
    defer mutex.Unlock()
    l.colorRegexp = rgx
}

func (l *Logger) SetTermWidth(width int) {
    mutex.Lock()
    defer mutex.Unlock()
    getWriterState(l.out).termWidth = width
}

// func (l *Logger) SetColorTemplate(str string) {
//     var rgx = str.replace
//     l.SetColorTemplateRegexp
// }




// SetOutput sets the output destination for the standard logger.
func SetOutput(w io.Writer) {
    mutex.Lock()
    defer mutex.Unlock()
    std.out = w
}

// Flags returns the output flags for the standard logger.
func Flags() int {
    return std.Flags()
}

// SetFlags sets the output flags for the standard logger.
func SetFlags(flag int) {
    std.SetFlags(flag)
}

// Prefix returns the output prefix for the standard logger.
func Prefix() string {
    return std.Prefix()
}

// SetPrefix sets the output prefix for the standard logger.
func SetPrefix(prefix string) {
    std.SetPrefix(prefix)
}

// These functions write to the standard logger.

// Print calls Output to print to the standard logger.
// Arguments are handled in the manner of fmt.Print.
func Print(v ...interface{}) {
    std.Output(2, fmt.Sprint(v...))
}

// Printf calls Output to print to the standard logger.
// Arguments are handled in the manner of fmt.Printf.
func Printf(format string, v ...interface{}) {
    std.Output(2, fmt.Sprintf(format, v...))
}

// Println calls Output to print to the standard logger.
// Arguments are handled in the manner of fmt.Println.
func Println(v ...interface{}) {
    std.Output(2, fmt.Sprintln(v...))
}

// Fatal is equivalent to Print() followed by a call to os.Exit(1).
func Fatal(v ...interface{}) {
    std.Output(2, fmt.Sprint(v...))
    os.Exit(1)
}

// Fatalf is equivalent to Printf() followed by a call to os.Exit(1).
func Fatalf(format string, v ...interface{}) {
    std.Output(2, fmt.Sprintf(format, v...))
    os.Exit(1)
}

// Fatalln is equivalent to Println() followed by a call to os.Exit(1).
func Fatalln(v ...interface{}) {
    std.Output(2, fmt.Sprintln(v...))
    os.Exit(1)
}

// Panic is equivalent to Print() followed by a call to panic().
func Panic(v ...interface{}) {
    s := fmt.Sprint(v...)
    std.Output(2, s)
    panic(s)
}

// Panicf is equivalent to Printf() followed by a call to panic().
func Panicf(format string, v ...interface{}) {
    s := fmt.Sprintf(format, v...)
    std.Output(2, s)
    panic(s)
}

// Panicln is equivalent to Println() followed by a call to panic().
func Panicln(v ...interface{}) {
    s := fmt.Sprintln(v...)
    std.Output(2, s)
    panic(s)
}

func ShowPartialLines() { std.ShowPartialLines() }
func HidePartialLines() { std.HidePartialLines() }
func EnableColor() { std.EnableColor() }
func DisableColor() { std.DisableColor() }
func EnableColorTemplate() { std.EnableColorTemplate() }
func DisableColorTemplate() { std.DisableColorTemplate() }
func SetColorTemplateRegexp(rgx *regexp.Regexp) { std.SetColorTemplateRegexp(rgx) }
func SetTermWidth(width int) { std.SetTermWidth(width) }

func AddAnsiCode(s string, code int) {
    ansiColorCodes[s] = code
}

// Output writes the output for a logging event.  The string s contains
// the text to print after the prefix specified by the flags of the
// Logger.  A newline is appended if the last character of s is not
// already a newline.  Calldepth is the count of the number of
// frames to skip when computing the file name and line number
// if Llongfile or Lshortfile is set; a value of 1 will print the details
// for the caller of Output.
func Output(calldepth int, s string) error {
    return std.Output(calldepth+1, s) // +1 for this frame.
}

package scale

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"go.bug.st/serial"
)

// Source opens the line to one indicator: a TCP port on a serial device server
// (the yard's Lantronix UDS2100) or a local serial port.
type Source interface {
	open(ctx context.Context, wait time.Duration) (line, error)
	String() string
}

// line is an open connection whose reads are bounded by the wait set at open:
// a read that times out returns no bytes and no error.
type line interface {
	read(p []byte) (n int, err error)
	io.Closer
}

// TCPSource reads an indicator through a serial-to-Ethernet box.
type TCPSource struct{ Address string }

func (s TCPSource) String() string { return "tcp://" + s.Address }

func (s TCPSource) open(ctx context.Context, wait time.Duration) (line, error) {
	d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
	c, err := d.DialContext(ctx, "tcp", s.Address)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "refused") {
			// What the UDS2100 does while another program holds the port.
			return nil, fmt.Errorf("%s refused the connection; another program may be holding this scale's line", s.Address)
		}
		return nil, err
	}
	return &tcpLine{c: c, wait: wait}, nil
}

type tcpLine struct {
	c    net.Conn
	wait time.Duration
}

func (l *tcpLine) read(p []byte) (int, error) {
	if err := l.c.SetReadDeadline(time.Now().Add(l.wait)); err != nil {
		return 0, err
	}
	n, err := l.c.Read(p)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return n, nil
	}
	return n, err
}

func (l *tcpLine) Close() error { return l.c.Close() }

// SerialSource reads an indicator cabled to this PC, for example through a
// splitter and a USB serial adapter.
type SerialSource struct {
	Port string
	Mode serial.Mode
}

func (s SerialSource) String() string { return "serial://" + s.Port }

func (s SerialSource) open(_ context.Context, wait time.Duration) (line, error) {
	mode := s.Mode
	p, err := serial.Open(s.Port, &mode)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Port, err) // the library's errors don't name the port
	}
	if err := p.SetReadTimeout(wait); err != nil {
		p.Close()
		return nil, err
	}
	return &serialLine{p: p}, nil
}

type serialLine struct{ p serial.Port }

// go.bug.st/serial already reports a read timeout as zero bytes and no error.
func (l *serialLine) read(b []byte) (int, error) { return l.p.Read(b) }

func (l *serialLine) Close() error { return l.p.Close() }

// Event is what Run reports: a decoded line (Frame), or why the scale cannot
// be read right now (Err). Exactly one of the two is set.
type Event struct {
	At    time.Time
	Frame *Frame
	Err   error
}

// Options bound how long Run waits on a quiet or unreachable scale.
type Options struct {
	// Stale is how long without a line before the scale is reported offline
	// while its connection stays open (indicator off, cable out).
	Stale time.Duration
	// Reconnect is how long without a line before the connection itself is
	// dropped and opened again, to recover from a link that died silently.
	Reconnect time.Duration
}

// backoff is the wait between attempts to open a line that keeps failing.
var backoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}

// Run reads src until ctx ends, reopening it forever. emit is called for every
// line and for every failure to open or keep the line, from this goroutine.
//
// The wait before reopening grows with each attempt that produced no reading,
// whether the open failed or the line opened and then dropped, so a device
// server that accepts and at once closes every connection is not hammered.
func Run(ctx context.Context, src Source, opt Options, emit func(Event)) {
	failures := 0
	for ctx.Err() == nil {
		l, err := src.open(ctx, opt.Stale)
		if err == nil {
			var read bool
			read, err = readLines(ctx, l, opt, emit)
			l.Close()
			if read {
				failures = 0
			}
		}
		if ctx.Err() != nil {
			return
		}
		emit(Event{At: time.Now(), Err: err})
		sleep(ctx, backoff[min(failures, len(backoff)-1)])
		failures++
	}
}

// maxLine bounds a run of bytes with no line ending, so noise on the line can
// never grow the buffer without limit.
const maxLine = 256

// ErrQuiet is the error while a line is open but no reading arrives (indicator
// off, cable out, wrong output setting). It is also what ends the connection
// after Options.Reconnect, so the reported error stays the same across the
// reconnects of a scale that is off all night.
var ErrQuiet = errors.New("connected, but the scale is sending no readings")

// droppedError is a line that failed mid-read. Its text is fixed (the cause
// names this PC's ephemeral port, different every time), so a line that keeps
// dropping reads as one ongoing problem rather than a new one each time.
type droppedError struct{ cause error }

func (e droppedError) Error() string {
	if errors.Is(e.cause, io.EOF) {
		return "the device server closed the connection"
	}
	return "the connection to the scale dropped"
}

func (e droppedError) Unwrap() error { return e.cause }

// readLines reads l until it fails or goes quiet for opt.Reconnect, and
// reports whether any reading arrived.
func readLines(ctx context.Context, l line, opt Options, emit func(Event)) (read bool, err error) {
	// Close the line when ctx ends so a blocked read returns.
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()

	buf := make([]byte, 0, 2*maxLine)
	chunk := make([]byte, 512)
	lastLine := time.Now()
	stale := false
	for {
		// The quiet check runs after every read, timed out or not, so a line
		// sending only noise goes stale like a silent one.
		n, err := l.read(chunk)
		now := time.Now()
		if n > 0 {
			buf = consume(buf, chunk[:n], func(f Frame) {
				lastLine, stale, read = now, false, true
				emit(Event{At: now, Frame: &f})
			})
		}
		if err != nil {
			return read, droppedError{err}
		}
		quiet := now.Sub(lastLine)
		if quiet >= opt.Reconnect {
			return read, ErrQuiet
		}
		if !stale && quiet >= opt.Stale {
			stale = true
			emit(Event{At: now, Err: ErrQuiet})
		}
	}
}

// consume appends data to buf, hands every complete line that parses to found,
// and returns what is left over: the start of the next line, if any.
func consume(buf, data []byte, found func(Frame)) []byte {
	buf = append(buf, data...)
	start := 0
	for {
		i := bytes.IndexByte(buf[start:], '\n')
		if i < 0 {
			break
		}
		f, err := ParseFrame(buf[start : start+i+1])
		start += i + 1
		if err == nil {
			found(f)
		}
	}
	rest := buf[start:]
	if len(rest) > maxLine {
		// Keep only from the last STX: the start of the next line, if any.
		if j := bytes.LastIndexByte(rest, stx); j >= 0 && len(rest)-j <= maxLine {
			rest = rest[j:]
		} else {
			rest = rest[:0]
		}
	}
	return buf[:copy(buf, rest)]
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

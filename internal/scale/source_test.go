package scale

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake device-server port: sends lines split across tiny writes, the way the
// UDS2100 delivered them (often one byte per TCP segment).
func TestRunReadsLinesSplitAcrossWrites(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		stream := "noise\x02      00LGZ\r\n\x02    2840LGM\r\n\x02    2840LG \r\n"
		for i := range len(stream) {
			c.Write([]byte{stream[i]})
			time.Sleep(time.Millisecond)
		}
		time.Sleep(time.Second)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var mu sync.Mutex
	var frames []Frame
	go Run(ctx, TCPSource{Address: ln.Addr().String()}, Options{Stale: time.Second, Reconnect: time.Minute}, func(e Event) {
		if e.Frame != nil {
			mu.Lock()
			frames = append(frames, *e.Frame)
			if len(frames) == 3 {
				cancel()
			}
			mu.Unlock()
		}
	})
	<-ctx.Done()

	mu.Lock()
	defer mu.Unlock()
	want := []Frame{
		{0, "lb", "gross", StatusZero},
		{2840, "lb", "gross", StatusMotion},
		{2840, "lb", "gross", StatusOK},
	}
	if len(frames) != len(want) {
		t.Fatalf("got %d frames %+v, want %d", len(frames), frames, len(want))
	}
	for i := range want {
		if frames[i] != want[i] {
			t.Errorf("frame %d = %+v, want %+v", i, frames[i], want[i])
		}
	}
}

func TestRunReportsRefusedAndQuietLines(t *testing.T) {
	// Refused: a port nothing listens on.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got error
	Run(ctx, TCPSource{Address: addr}, Options{Stale: time.Second, Reconnect: time.Minute}, func(e Event) {
		if e.Frame == nil && got == nil {
			got = e.Err
			cancel()
		}
	})
	if got == nil || !strings.Contains(got.Error(), "another program may be holding") {
		t.Fatalf("refused connection reported as %v", got)
	}

	// Quiet: connected, but the indicator sends nothing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(3 * time.Second)
		}
	}()
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got = nil
	Run(ctx, TCPSource{Address: ln.Addr().String()}, Options{Stale: 300 * time.Millisecond, Reconnect: time.Minute}, func(e Event) {
		if e.Frame == nil && got == nil {
			got = e.Err
			cancel()
		}
	})
	if !errors.Is(got, ErrQuiet) {
		t.Fatalf("quiet line reported as %v", got)
	}
}

// A line that sends only bytes that never parse is as useless as a silent one.
func TestReadLinesNoiseGoesStale(t *testing.T) {
	l := &fakeLine{reads: func(int) ([]byte, error) {
		time.Sleep(10 * time.Millisecond)
		return []byte("\x02garbage\r\n"), nil
	}}
	var got error
	read, err := readLines(context.Background(), l, Options{Stale: 100 * time.Millisecond, Reconnect: 300 * time.Millisecond}, func(e Event) {
		if e.Err != nil && got == nil {
			got = e.Err
		}
	})
	if !errors.Is(got, ErrQuiet) {
		t.Errorf("noise reported as %v", got)
	}
	if !errors.Is(err, ErrQuiet) || read {
		t.Errorf("noise: read=%v err=%v, want no reading and a reconnect", read, err)
	}
}

// A read may return the last lines together with the error that ended the line.
func TestReadLinesKeepsDataReturnedWithAnError(t *testing.T) {
	l := &fakeLine{reads: func(i int) ([]byte, error) {
		return []byte("\x02    4120LG \r\n"), io.EOF
	}}
	var frames int
	read, err := readLines(context.Background(), l, Options{Stale: time.Second, Reconnect: time.Minute}, func(e Event) {
		if e.Frame != nil {
			frames++
		}
	})
	if frames != 1 || !read || !errors.Is(err, io.EOF) || err.Error() != "the device server closed the connection" {
		t.Errorf("got %d frames, read=%v, %v; want 1 frame and the closed-connection error", frames, read, err)
	}
}

// A device server that accepts every connection and closes it at once must
// be retried with a growing wait, and reported with one unchanging message.
func TestRunBacksOffALineThatOpensAndDrops(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var accepts []time.Time
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepts = append(accepts, time.Now())
			mu.Unlock()
			c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()
	msgs := map[string]bool{}
	Run(ctx, TCPSource{Address: ln.Addr().String()}, Options{Stale: time.Second, Reconnect: time.Minute}, func(e Event) {
		if e.Err != nil {
			msgs[e.Err.Error()] = true
		}
	})
	mu.Lock()
	defer mu.Unlock()
	// Waits of 1 s then 2 s: connections at about 0, 1 and 3 s, never one a second.
	if len(accepts) < 2 || len(accepts) > 3 {
		t.Errorf("%d connections in 3.5 s, want 2 or 3", len(accepts))
	}
	if len(msgs) != 1 {
		t.Errorf("messages %v, want one unchanging message", msgs)
	}
}

type fakeLine struct {
	n     int
	reads func(i int) ([]byte, error)
}

func (l *fakeLine) read(p []byte) (int, error) {
	b, err := l.reads(l.n)
	l.n++
	return copy(p, b), err
}

func (l *fakeLine) Close() error { return nil }

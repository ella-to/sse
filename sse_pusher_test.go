package sse

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// stallingWriter models a peer that stopped reading: Write blocks until the
// write deadline (set through http.ResponseController) passes.
type stallingWriter struct {
	header   http.Header
	mu       sync.Mutex
	deadline time.Time
	changed  chan struct{}
}

func newStallingWriter() *stallingWriter {
	return &stallingWriter{header: make(http.Header), changed: make(chan struct{})}
}

func (w *stallingWriter) Header() http.Header        { return w.header }
func (w *stallingWriter) WriteHeader(statusCode int) {}
func (w *stallingWriter) Flush()                     {}

func (w *stallingWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.deadline = t
	close(w.changed)
	w.changed = make(chan struct{})
	w.mu.Unlock()
	return nil
}

func (w *stallingWriter) Write(b []byte) (int, error) {
	for {
		w.mu.Lock()
		d, ch := w.deadline, w.changed
		w.mu.Unlock()

		var timeout <-chan time.Time
		if !d.IsZero() {
			t := time.NewTimer(time.Until(d))
			timeout = t.C
			select {
			case <-timeout:
				t.Stop()
				return 0, os.ErrDeadlineExceeded
			case <-ch:
				t.Stop()
			}
			continue
		}
		<-ch
	}
}

func TestPusherWriteTimeout(t *testing.T) {
	w := newStallingWriter()
	p, err := CreateHttpPusher(w, WithHttpPusherWriteTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	start := time.Now()
	err = p.Push(&Message{Data: "x"})
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Push error = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Push took %v", d)
	}
}

func TestPusherCloseUnblocksStalledWrite(t *testing.T) {
	w := newStallingWriter()
	p, err := CreateHttpPusher(w) // no write timeout configured
	if err != nil {
		t.Fatal(err)
	}

	pushed := make(chan error, 1)
	go func() { pushed <- p.Push(&Message{Data: "x"}) }()
	time.Sleep(20 * time.Millisecond)

	closed := make(chan struct{})
	go func() { _ = p.Close(); close(closed) }()

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind a stalled write")
	}
	select {
	case err := <-pushed:
		if err == nil {
			t.Fatal("stalled Push returned nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled Push never returned")
	}
}

type flushErrorWriter struct {
	httptest.ResponseRecorder
}

func (w *flushErrorWriter) FlushError() error { return errors.New("flush failed") }

func TestPusherReportsFlushError(t *testing.T) {
	w := &flushErrorWriter{ResponseRecorder: *httptest.NewRecorder()}
	if _, err := CreateHttpPusher(w); err == nil || err.Error() != "flush failed" {
		t.Fatalf("CreateHttpPusher error = %v, want flush failed", err)
	}
}

// unwrappingWriter hides the Flusher behind Unwrap, as middleware does.
type unwrappingWriter struct {
	inner http.ResponseWriter
}

func (w *unwrappingWriter) Header() http.Header         { return w.inner.Header() }
func (w *unwrappingWriter) Write(b []byte) (int, error) { return w.inner.Write(b) }
func (w *unwrappingWriter) WriteHeader(code int)        { w.inner.WriteHeader(code) }
func (w *unwrappingWriter) Unwrap() http.ResponseWriter { return w.inner }

func TestPusherThroughUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	p, err := CreateHttpPusher(&unwrappingWriter{inner: rec})
	if err != nil {
		t.Fatalf("CreateHttpPusher through Unwrap: %v", err)
	}
	if err := p.Push(&Message{Id: "1", Data: "x"}); err != nil {
		t.Fatal(err)
	}
	if !rec.Flushed {
		t.Fatal("not flushed")
	}
}

type noFlushWriter struct{ header http.Header }

func (w *noFlushWriter) Header() http.Header         { return w.header }
func (w *noFlushWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *noFlushWriter) WriteHeader(code int)        {}

func TestPusherRequiresFlush(t *testing.T) {
	if _, err := CreateHttpPusher(&noFlushWriter{header: make(http.Header)}); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("error = %v, want ErrNotSupported", err)
	}
}

// countingFlushWriter records writes and how many flushes were requested.
type countingFlushWriter struct {
	httptest.ResponseRecorder
	flushes int
}

func (w *countingFlushWriter) Flush() { w.flushes++ }

func TestPusherWriteThenFlush(t *testing.T) {
	w := &countingFlushWriter{ResponseRecorder: *httptest.NewRecorder()}
	p, err := CreateHttpPusher(w)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	base := w.flushes // CreateHttpPusher flushes the headers

	if err := p.WriteMsg(&Message{Id: "1", Data: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteBytes([]byte("2"), "", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if w.flushes != base {
		t.Fatalf("Write* flushed %d times", w.flushes-base)
	}
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	if w.flushes != base+1 {
		t.Fatalf("flushes = %d, want 1", w.flushes-base)
	}
	if got := w.Body.String(); got != "id: 1\ndata: a\n\nid: 2\ndata: b\n\n" {
		t.Fatalf("body = %q", got)
	}

	p.Close()
	if err := p.WriteMsg(&Message{Data: "x"}); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("WriteMsg after Close = %v", err)
	}
	if err := p.Flush(); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Flush after Close = %v", err)
	}
}

func TestPusherEncoded(t *testing.T) {
	w := &countingFlushWriter{ResponseRecorder: *httptest.NewRecorder()}
	p, err := CreateHttpPusher(w)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	base := w.flushes

	frame, err := AppendMessage(nil, []byte("1"), "msg", []byte("shared"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.WriteEncoded(frame); err != nil {
		t.Fatal(err)
	}
	if w.flushes != base {
		t.Fatal("WriteEncoded flushed")
	}
	if err := p.PushEncoded(frame); err != nil {
		t.Fatal(err)
	}
	if w.flushes != base+1 {
		t.Fatal("PushEncoded did not flush")
	}
	want := "id: 1\nevent: msg\ndata: shared\n\n"
	if got := w.Body.String(); got != want+want {
		t.Fatalf("body = %q", got)
	}
}

func TestPusherPingOnlyWhenIdle(t *testing.T) {
	w := &recordingResponseWriter{}
	p, err := CreateHttpPusher(w, WithHttpPusherPingDuration(60*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// steady traffic faster than the ping duration: no pings
	for i := 0; i < 20; i++ {
		if err := p.Push(&Message{Id: "1", Data: "x"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out, _ := w.snapshot(); strings.Contains(out, ": ping") {
		t.Fatalf("ping sent on a busy connection: %q", out)
	}

	// idle: a ping must arrive within roughly one ping duration
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if out, _ := w.snapshot(); strings.Contains(out, ": ping\n\n") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no ping on an idle connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

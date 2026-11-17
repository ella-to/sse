package sse

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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

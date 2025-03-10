package sse

import (
	"io"
	"net/http"
	"sync"
	"time"
)

//
// PushWriter
//

type PushWriter struct {
	w            io.Writer
	mtx          sync.Mutex
	timeout      time.Duration
	clearTimeout func()
}

func (p *PushWriter) Push(msg *Message) error {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.clearTimeout != nil && p.timeout > 0 {
		p.clearTimeout()
		p.clearTimeout = setTimeout(p.timeout, p.ping)
	}

	_, err := io.Copy(p.w, msg)
	if err != nil {
		return err
	}

	return nil
}

func (p *PushWriter) Close() error {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.clearTimeout != nil {
		p.clearTimeout()
		p.clearTimeout = nil
	}

	return nil
}

func (p *PushWriter) ping() {
	p.Push(NewPingEvent())
}

func NewPushWriter(w io.Writer, timeout time.Duration) *PushWriter {
	return &PushWriter{
		w:       w,
		timeout: timeout,
	}
}

//
// Http Pusher
//

func NewHttpPusher(w http.ResponseWriter, timeout time.Duration) (Pusher, error) {
	out, ok := w.(http.Flusher)
	if !ok {
		return nil, http.ErrNotSupported
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	out.Flush() // Flush the headers
	engine := NewPushWriter(w, timeout)

	return NewPushCloser(
		func(msg *Message) error {
			if err := engine.Push(msg); err != nil {
				return err
			}
			out.Flush()
			return nil
		},
		engine.Close,
	), nil
}

//
// Helpers
//

func setTimeout(delay time.Duration, fn func()) func() {
	if delay <= 0 {
		return func() {}
	}

	timer := time.NewTimer(delay)

	// Create a cancel channel
	cancel := make(chan struct{})

	go func() {
		select {
		case <-timer.C:
			// Timer expired, execute the function
			fn()
		case <-cancel:
			// Timer was canceled
			timer.Stop()
		}
	}()

	// Return the cancel function
	return func() {
		close(cancel)
	}
}

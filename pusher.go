package sse

import (
	"io"
	"net/http"
	"sync"
	"time"
)

//
// rawPusher
//

type rawPusher struct {
	w            io.Writer
	mtx          sync.Mutex
	timeout      time.Duration
	clearTimeout func()
	closed       bool // Add closed flag
}

func (p *rawPusher) Push(msg *Message) error {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.closed { // Prevent push if closed
		return io.ErrClosedPipe
	}

	if p.timeout > 0 {
		if p.clearTimeout != nil {
			p.clearTimeout()
		}
		p.clearTimeout = setTimeout(p.timeout, p.ping)
	}

	_, err := io.Copy(p.w, msg)
	if err != nil {
		return err
	}

	return nil
}

func (p *rawPusher) Close() error {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true

	if p.clearTimeout != nil {
		p.clearTimeout()
		p.clearTimeout = nil
	}

	return nil
}

func (p *rawPusher) ping() {
	p.mtx.Lock()
	if p.closed {
		p.mtx.Unlock()
		return
	}
	p.mtx.Unlock()
	p.Push(NewPingEvent())
}

func NewPusher(w io.Writer, timeout time.Duration) (Pusher, error) {
	switch v := w.(type) {
	case http.ResponseWriter:
		return NewHttpPusher(v, timeout)
	default:
		return &rawPusher{w: w, timeout: timeout}, nil
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

	raw := &rawPusher{w: w, timeout: timeout}

	return NewPushCloser(
		func(msg *Message) error {
			if err := raw.Push(msg); err != nil {
				return err
			}
			out.Flush()
			return nil
		},
		raw.Close,
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

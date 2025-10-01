package sse

import (
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

//
// rawPusher
//

type rawPusher struct {
	w       io.Writer
	mtx     sync.RWMutex // Use RWMutex for better read performance
	timeout time.Duration
	timer   *time.Timer
	closed  int32 // Use atomic for lock-free reads
	done    chan struct{}
}

func (p *rawPusher) Push(msg *Message) error {
	// Fast path: check if closed without lock
	if atomic.LoadInt32(&p.closed) == 1 {
		return io.ErrClosedPipe
	}

	p.mtx.Lock()
	defer p.mtx.Unlock()

	// Double-check after acquiring lock
	if atomic.LoadInt32(&p.closed) == 1 {
		return io.ErrClosedPipe
	}

	// Manage timer more efficiently
	if p.timeout > 0 {
		if p.timer == nil {
			p.timer = time.NewTimer(p.timeout)
			go p.timerHandler()
		} else {
			p.timer.Reset(p.timeout)
		}
	}

	_, err := io.Copy(p.w, msg)
	return err
}

func (p *rawPusher) Close() error {
	// Use atomic to prevent double-close
	if !atomic.CompareAndSwapInt32(&p.closed, 0, 1) {
		return nil // Already closed
	}

	p.mtx.Lock()
	if p.timer != nil {
		p.timer.Stop()
	}
	close(p.done) // Signal goroutine to stop
	p.mtx.Unlock()

	return nil
}

// timerHandler manages the ping timer in a single goroutine
func (p *rawPusher) timerHandler() {
	for {
		select {
		case <-p.timer.C:
			// Check if we should exit after timer fires
			if atomic.LoadInt32(&p.closed) == 1 {
				return
			}

			// Send ping without recursion
			p.sendPing()

			// Reset timer for next ping
			if p.timeout > 0 && atomic.LoadInt32(&p.closed) == 0 {
				p.timer.Reset(p.timeout)
			}
		case <-p.done:
			// Exit when Close() is called
			return
		}
	}
}

func (p *rawPusher) sendPing() {
	p.mtx.RLock()
	closed := atomic.LoadInt32(&p.closed) == 1
	p.mtx.RUnlock()

	if !closed {
		p.Push(NewPingEvent())
	}
}

func NewPusher(w io.Writer, timeout time.Duration) (Pusher, error) {
	switch v := w.(type) {
	case http.ResponseWriter:
		return NewHttpPusher(v, timeout)
	default:
		return &rawPusher{w: w, timeout: timeout, done: make(chan struct{})}, nil
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
	w.Header().Set("Access-Control-Allow-Origin", "*") // Add CORS support
	w.Header().Set("Access-Control-Allow-Headers", "Cache-Control")

	out.Flush() // Flush the headers

	raw := &rawPusher{w: w, timeout: timeout, done: make(chan struct{})}

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

package sse

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"time"
)

var ping = &Ping{}

type pusher struct {
	w            io.Writer
	out          http.Flusher
	mtx          sync.Mutex
	clearTimeout func()
	timeout      time.Duration
	buffer       bytes.Buffer
}

var _ Pusher = &pusher{}

func (p *pusher) Push(msg *Message) error {
	return p.push(msg)
}

func (p *pusher) Close() {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.clearTimeout != nil {
		p.clearTimeout()
		p.clearTimeout = nil
	}
}

func (p *pusher) push(enc StringEncoder) error {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	p.buffer.Reset()
	enc.EncodeString(&p.buffer)

	if p.clearTimeout != nil && p.timeout > 0 {
		p.clearTimeout()
		p.clearTimeout = setTimeout(p.timeout, p.ping)
	}

	_, err := p.w.Write(p.buffer.Bytes())
	if err != nil {
		return err
	}

	p.out.Flush()

	return nil
}

func (p *pusher) ping() {
	p.push(ping)
}

func NewPusher(w http.ResponseWriter, timeout time.Duration) (Pusher, error) {
	out, ok := w.(http.Flusher)
	if !ok {
		return nil, http.ErrNotSupported
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	out.Flush()

	p := &pusher{
		w:   w,
		out: out,
	}

	if timeout > 0 {
		p.timeout = timeout
		p.clearTimeout = setTimeout(p.timeout, p.ping)
	}

	return p, nil
}

func setTimeout(delay time.Duration, fn func()) func() {
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

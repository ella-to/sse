package sse

import (
	"io"
	"net/http"
)

type pusher struct {
	w   io.Writer
	out http.Flusher
}

var _ Pusher = &pusher{}

func (p *pusher) Push(msg *Message) error {
	_, err := p.w.Write([]byte(msg.String()))
	if err != nil {
		return err
	}

	p.out.Flush()

	return nil
}

func NewPusher(w http.ResponseWriter) (Pusher, error) {
	out, ok := w.(http.Flusher)
	if !ok {
		return nil, http.ErrNotSupported
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	out.Flush()

	return &pusher{
		w:   w,
		out: out,
	}, nil
}

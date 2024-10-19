package sse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type Pusher interface {
	Push(ctx context.Context, id string, data any) error
	Done(ctx context.Context) error
}

type pusher struct {
	w   io.Writer
	out http.Flusher
	id  int
}

var _ Pusher = (*pusher)(nil)

func (p *pusher) Push(ctx context.Context, event string, data any) error {
	p.id++

	if err := WriteEvent(p.w, p.id, event, data); err != nil {
		return err
	}

	p.out.Flush()

	return nil
}

func (p *pusher) Done(ctx context.Context) error {
	return p.Push(ctx, "done", struct{}{})
}

type pusherOptions struct {
	headers map[string]string
}

type OptionFunc func(*pusherOptions)

func WithHeader(key, value string) OptionFunc {
	return func(o *pusherOptions) {
		if o.headers == nil {
			o.headers = make(map[string]string)
		}
		o.headers[key] = value
	}
}

func CreatePusher(w http.ResponseWriter, argsFns ...OptionFunc) (*pusher, error) {
	opts := &pusherOptions{
		headers: make(map[string]string),
	}
	for _, fn := range argsFns {
		fn(opts)
	}

	out, ok := w.(http.Flusher)
	if !ok {
		return nil, http.ErrNotSupported
	}

	for key, value := range opts.headers {
		w.Header().Set(key, value)
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

func WriteEvent(w io.Writer, id int, event string, data any) error {
	if err, ok := data.(error); ok {
		data = struct {
			Error string `json:"error"`
		}{
			Error: err.Error(),
		}
		event = "error"
	}

	var err error

	_, err = w.Write(idPrefix)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(strconv.Itoa(id)))
	if err != nil {
		return err
	}
	_, err = w.Write(singleEnter)
	if err != nil {
		return err
	}

	_, err = w.Write(eventPrefix)
	if err != nil {
		return err
	}

	_, err = w.Write([]byte(event))
	if err != nil {
		return err
	}

	_, err = w.Write(singleEnter)
	if err != nil {
		return err
	}

	_, err = w.Write(dataPrefix)
	if err != nil {
		return err
	}

	if v, ok := data.(string); ok {
		// NOTE: because SSE requires to have 2 new lines to separate the event
		// all the data should be trimmed to avoid any extra new lines
		_, err = w.Write([]byte(strings.TrimSpace(v)))
		if err != nil {
			return err
		}
		_, err = w.Write(doubleEnters)
		if err != nil {
			return err
		}
	} else {
		// NOTE: json.NewEncode will add a new line at the end of the data
		// as a result, we need to add just one new line to separate the event
		err := json.NewEncoder(w).Encode(data)
		if err != nil {
			return err
		}
		_, err = w.Write(singleEnter)
		if err != nil {
			return err
		}
	}
	return err
}

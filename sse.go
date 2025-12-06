package sse

import (
	"context"
	"log/slog"
)

const (
	Version = "0.1.2"
)

//
// Pusher
//

type Pusher interface {
	Push(msg *Message) error
	Close() error
}

//
// Receiver
//

type Receiver interface {
	Receive(ctx context.Context) (*Message, error)
}

//
// PushCloser
//

type pushCloser struct {
	push  func(msg *Message) error
	close func() error
}

func (pc *pushCloser) Push(msg *Message) error {
	return pc.push(msg)
}

func (pc *pushCloser) Close() error {
	return pc.close()
}

func NewPushCloser(push func(msg *Message) error, close func() error) Pusher {
	return &pushCloser{
		push:  push,
		close: close,
	}
}

var logger = slog.Default()

func SetLogger(l *slog.Logger) {
	logger = l
}

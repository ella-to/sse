package sse

import (
	"context"
	"strings"
)

const (
	Version = "0.0.7"
)

type Pusher interface {
	Push(msg *Message) error
	Close()
}

type Receiver interface {
	Receive(ctx context.Context) (*Message, error)
}

type StringWriter interface {
	WriteString(string) (int, error)
}

type StringEncoder interface {
	EncodeString(StringWriter)
}

type Message struct {
	Id    *string
	Event string
	Data  *string
}

var _ StringEncoder = (*Message)(nil)

func (m *Message) EncodeString(sw StringWriter) {
	if m.Id != nil {
		sw.WriteString("id: ")
		sw.WriteString(*m.Id)
		sw.WriteString("\n")
	}

	if m.Event != "" {
		sw.WriteString("event: ")
		sw.WriteString(m.Event)
		sw.WriteString("\n")
	}

	if m.Data != nil {
		sw.WriteString("data: ")
		sw.WriteString(*m.Data)
		sw.WriteString("\n")
	}

	sw.WriteString("\n")
}

func (m *Message) String() string {
	var sb strings.Builder

	m.EncodeString(&sb)

	return sb.String()
}

type Ping struct{}

var _ StringEncoder = (*Ping)(nil)

func (p *Ping) EncodeString(sw StringWriter) {
	sw.WriteString(": ping\n\n")
}

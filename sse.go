package sse

import (
	"context"
	"strings"
)

type Pusher interface {
	Push(msg *Message) error
}

type Receiver interface {
	Receive(ctx context.Context) (*Message, error)
}

type Message struct {
	Id    *string
	Event string
	Data  *string
}

func (m *Message) String() string {
	var sb strings.Builder

	if m.Id != nil {
		sb.WriteString("id: ")
		sb.WriteString(*m.Id)
		sb.WriteString("\n")
	}

	if m.Event != "" {
		sb.WriteString("event: ")
		sb.WriteString(m.Event)
		sb.WriteString("\n")
	}

	if m.Data != nil {
		sb.WriteString("data: ")
		sb.WriteString(*m.Data)
		sb.WriteString("\n")
	}

	sb.WriteString("\n")

	return sb.String()
}

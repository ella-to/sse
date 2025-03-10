package sse

import (
	"io"
)

type Message struct {
	Id    string
	Event string
	Data  string

	// private for keep track of Reader state
	readerRemaining int
	buffer          []byte
}

func (m *Message) String() string {
	return "id: " + m.Id + ", event: " + m.Event + ", data: " + m.Data
}

func (m *Message) Read(b []byte) (int, error) {
	if m.readerRemaining == 0 {
		if m.Id != "" {
			m.buffer = append(m.buffer, "id: "...)
			m.buffer = append(m.buffer, m.Id...)
			m.buffer = append(m.buffer, '\n')
		}

		if m.Event != "" {
			m.buffer = append(m.buffer, "event: "...)
			m.buffer = append(m.buffer, m.Event...)
			m.buffer = append(m.buffer, '\n')
		}

		if m.Data != "" {
			m.buffer = append(m.buffer, "data: "...)
			m.buffer = append(m.buffer, m.Data...)
			m.buffer = append(m.buffer, '\n')
		}

		if len(m.buffer) == 0 {
			m.buffer = []byte(": ping\n\n")
		}

		// End with an extra newline to complete the event
		m.buffer = append(m.buffer, '\n')
	}

	n := copy(b, m.buffer)
	m.readerRemaining = len(m.buffer) - n
	m.buffer = m.buffer[n:]

	if m.readerRemaining == 0 {
		return n, io.EOF
	}

	return n, nil
}

func (m *Message) Write(b []byte) (int, error) {
	m.Id = ""
	m.Event = ""
	m.Data = ""

	var i int
	for i < len(b) {
		// Find field name
		start := i
		for i < len(b) && b[i] != ':' && b[i] != '\n' {
			i++
		}

		if i >= len(b) || b[i] == '\n' {
			i++
			continue // Empty line or invalid format
		}

		fieldName := string(b[start:i])
		i++ // Skip the colon

		// Skip the space after colon if present
		if i < len(b) && b[i] == ' ' {
			i++
		}

		// Find field value
		start = i
		for i < len(b) && b[i] != '\n' {
			i++
		}

		value := string(b[start:i])
		i++ // Skip the newline

		// Process the field
		switch fieldName {
		case "id":
			m.Id = value
		case "event":
			m.Event = value
			// If ping event, reset all fields
			if value == "ping" {
				m.Id = ""
				m.Event = ""
				m.Data = ""
				return len(b), nil
			}
		case "data":
			m.Data = value
		}
	}

	return len(b), nil
}

func NewMessage(id, event, data string) *Message {
	return &Message{
		Id:    id,
		Event: event,
		Data:  data,
	}
}

func NewPingEvent() *Message {
	return &Message{}
}

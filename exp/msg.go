package exp

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
	lines := string(b)
	currentField := ""
	currentValue := ""

	// Reset the event fields
	m.Id = ""
	m.Event = ""
	m.Data = ""

	for i := 0; i < len(lines); i++ {
		// Check for line endings
		if lines[i] == '\n' {
			// Process the current line
			if currentField != "" {
				switch currentField {
				case "id":
					m.Id = currentValue
				case "event":
					m.Event = currentValue
					// If ping event, reset all fields
					if currentValue == "ping" {
						m.Id = ""
						m.Event = ""
						m.Data = ""
						return len(b), nil
					}
				case "data":
					m.Data = currentValue
				}
				currentField = ""
				currentValue = ""
			}
			continue
		}

		// Parse field name
		if currentField == "" && i+1 < len(lines) && lines[i] != ':' {
			j := i
			for j < len(lines) && lines[j] != ':' && lines[j] != '\n' {
				j++
			}
			if j < len(lines) && lines[j] == ':' {
				currentField = lines[i:j]
				i = j
				// Skip the space after colon if present
				if i+1 < len(lines) && lines[i+1] == ' ' {
					i++
				}
			}
		} else if currentField != "" {
			// Append to current value
			currentValue += string(lines[i])
		}
	}

	// Process any remaining field
	if currentField != "" {
		switch currentField {
		case "id":
			m.Id = currentValue
		case "event":
			m.Event = currentValue
			// If ping event, reset all fields
			if currentValue == "ping" {
				m.Id = ""
				m.Event = ""
				m.Data = ""
			}
		case "data":
			m.Data = currentValue
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

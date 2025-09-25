package sse

import (
	"io"
	"sync"
)

// Pool for reusing byte slices to reduce memory allocations
var bufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 256) // Smaller initial capacity
		return &buf
	},
}

// getBuffer gets a buffer from the pool
func getBuffer() []byte {
	return (*bufferPool.Get().(*[]byte))[:0] // Reset length to 0
}

// putBuffer returns a buffer to the pool
func putBuffer(buf []byte) {
	if cap(buf) <= 4096 { // Only pool buffers up to 4KB to prevent memory bloat
		bufferPool.Put(&buf)
	}
}

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

// Reset clears the message for reuse
func (m *Message) Reset() {
	m.Id = ""
	m.Event = ""
	m.Data = ""
	m.readerRemaining = 0
	if m.buffer != nil {
		putBuffer(m.buffer[:0])
		m.buffer = nil
	}
}

// SetMessage sets all fields at once for efficient reuse
func (m *Message) SetMessage(id, event, data string) {
	m.Reset()
	m.Id = id
	m.Event = event
	m.Data = data
}

func (m *Message) Read(b []byte) (int, error) {
	if m.readerRemaining == 0 {
		// Estimate required buffer size to avoid reallocations
		estimatedSize := len(m.Id) + len(m.Event) + len(m.Data) + 20 // 20 for prefixes and newlines

		if estimatedSize <= 64 {
			// Use stack allocation for small messages
			var stackBuffer [64]byte
			buf := stackBuffer[:0]

			if m.Id != "" {
				buf = append(buf, "id: "...)
				buf = append(buf, m.Id...)
				buf = append(buf, '\n')
			}

			if m.Event != "" {
				buf = append(buf, "event: "...)
				buf = append(buf, m.Event...)
				buf = append(buf, '\n')
			}

			if m.Data != "" {
				buf = append(buf, "data: "...)
				buf = append(buf, m.Data...)
				buf = append(buf, '\n')
			}

			if len(buf) == 0 {
				buf = append(buf, ": ping\n\n"...)
			} else {
				buf = append(buf, '\n')
			}

			n := copy(b, buf)
			if n < len(buf) {
				// Buffer too small, fall back to heap allocation
				m.buffer = getBuffer()
				m.buffer = append(m.buffer, buf[n:]...)
				m.readerRemaining = len(m.buffer)
				return n, nil
			}
			return n, io.EOF
		}

		// Use buffer pool for larger messages
		m.buffer = getBuffer()

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
			m.buffer = append(m.buffer, ": ping\n\n"...)
		} else {
			m.buffer = append(m.buffer, '\n')
		}
	}

	n := copy(b, m.buffer)
	m.readerRemaining = len(m.buffer) - n
	m.buffer = m.buffer[n:]

	if m.readerRemaining == 0 {
		// Return buffer to pool when done
		if m.buffer != nil {
			putBuffer(m.buffer[:0])
			m.buffer = nil
		}
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

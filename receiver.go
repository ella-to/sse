package sse

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
)

type receiver struct {
	ch <-chan *Message
}

var _ Receiver = &receiver{}

func (r *receiver) Receive(ctx context.Context) (*Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg, ok := <-r.ch:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	}
}

func NewReceiver(rc io.Reader) Receiver {
	return &receiver{
		ch: Parse(rc),
	}
}

func Parse(r io.Reader) <-chan *Message {
	ch := make(chan *Message, 16) // Buffered channel for better throughput
	scanner := bufio.NewScanner(r)

	// Use a larger buffer to reduce system calls
	buf := make([]byte, 0, 4096)
	scanner.Buffer(buf, 65536) // 64KB max token size

	go func() {
		defer close(ch)

		for {
			msg, err := parseMessageOptimized(scanner)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					// Log error if needed
				}
				return
			}

			// Skip empty messages
			if msg.Id == "" && msg.Event == "" && msg.Data == "" {
				continue
			}

			ch <- msg

			if msg.Event == "done" {
				return
			}
		}
	}()

	return ch
}

// parseMessageOptimized uses bufio.Scanner for efficient line reading
func parseMessageOptimized(scanner *bufio.Scanner) (*Message, error) {
	msg := &Message{}

	for scanner.Scan() {
		line := scanner.Text()

		// Empty line indicates end of message
		if line == "" {
			break
		}

		// Comment line (starts with :)
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse field: value pairs
		if colonIndex := strings.Index(line, ": "); colonIndex != -1 {
			field := line[:colonIndex]
			value := line[colonIndex+2:]

			switch field {
			case "id":
				msg.Id = value
			case "event":
				msg.Event = value
			case "data":
				msg.Data = value
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// If we got here without any fields, check if scanner is done
	if msg.Id == "" && msg.Event == "" && msg.Data == "" {
		return nil, io.EOF
	}

	return msg, nil
}

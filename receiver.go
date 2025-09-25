package sse

import (
	"bufio"
	"context"
	"errors"
	"io"
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
				PutMessage(msg) // Return unused message to pool
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
	msg := GetMessage() // Use pooled message

	for scanner.Scan() {
		line := scanner.Bytes() // Use Bytes() instead of Text() to avoid string allocation

		// Empty line indicates end of message
		if len(line) == 0 {
			break
		}

		// Comment line (starts with :)
		if len(line) > 0 && line[0] == ':' {
			continue
		}

		// Parse field: value pairs using byte operations
		colonIndex := -1
		for i, b := range line {
			if b == ':' && i+1 < len(line) && line[i+1] == ' ' {
				colonIndex = i
				break
			}
		}

		if colonIndex != -1 {
			field := line[:colonIndex]
			value := line[colonIndex+2:]

			// Use byte comparison to avoid string allocations
			if len(field) == 2 && field[0] == 'i' && field[1] == 'd' {
				msg.Id = string(value)
			} else if len(field) == 5 &&
				field[0] == 'e' && field[1] == 'v' && field[2] == 'e' &&
				field[3] == 'n' && field[4] == 't' {
				msg.Event = string(value)
			} else if len(field) == 4 &&
				field[0] == 'd' && field[1] == 'a' && field[2] == 't' && field[3] == 'a' {
				msg.Data = string(value)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		PutMessage(msg) // Return to pool on error
		return nil, err
	}

	// If we got here without any fields, check if scanner is done
	if msg.Id == "" && msg.Event == "" && msg.Data == "" {
		PutMessage(msg) // Return to pool
		return nil, io.EOF
	}

	return msg, nil
}

package sse

import (
	"bytes"
	"context"
	"errors"
	"io"
)

type receiver struct {
	ch    <-chan *Message
	close func() error
}

var _ Receiver = &receiver{}
var _ io.Closer = &receiver{}

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

func (r *receiver) Close() error {
	return r.close()
}

func NewReceiver(rc io.ReadCloser) Receiver {
	return &receiver{
		ch:    Parse(rc),
		close: rc.Close,
	}
}

func Parse(r io.Reader) <-chan *Message {
	ch := make(chan *Message)
	var buffer bytes.Buffer

	go func() {
		defer close(ch)
		for {
			msg, err := parseMessage(r, &buffer)
			done := errors.Is(err, io.EOF)

			if err != nil && !done {
				return
			}

			ch <- msg

			if done || msg.Event == "done" {
				return
			}
		}
	}()

	return ch
}

func parseMessage(r io.Reader, buffer *bytes.Buffer) (*Message, error) {
	msg := &Message{}

	lastMsgWasComment := false

	for {
		buffer.Reset()

		err := readLine(r, buffer)
		if err != nil {
			return msg, err
		}

		line := buffer.Bytes()

		if len(line) == 0 {
			if lastMsgWasComment {
				lastMsgWasComment = false
				continue
			}
			break
		}

		// it means that the line is a comment
		// and we can ignore it
		if line[0] == ':' {
			lastMsgWasComment = true
			continue
		}

		index := bytes.Index(line, []byte(": "))
		if index == -1 {
			// this shouldn't happen, but if it does
			// it means that the line is invalid
			// and we can ignore it
			continue
		}

		field := string(line[:index])
		value := string(line[index+2:])

		switch field {
		case "id":
			msg.Id = &value
		case "event":
			msg.Event = value
		case "data":
			msg.Data = &value
		}
	}

	return msg, nil
}

// fill the buffer with the content of the line
// until a newline character is found, it will eat the newline
// but it will not be part of the buffer
func readLine(r io.Reader, buffer *bytes.Buffer) error {
	b := make([]byte, 1)

	for {
		_, err := r.Read(b)
		if err != nil {
			return err
		}

		if b[0] == '\n' {
			return nil
		}

		buffer.Write(b)
	}
}

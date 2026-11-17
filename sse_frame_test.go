package sse

import (
	"bytes"
	"errors"
	"testing"
)

func TestWriteMessageRejectsLineBreaks(t *testing.T) {
	cases := []*Message{
		{Id: "e_1\nevent: error", Data: "x"},
		{Id: "e_1\r", Data: "x"},
		{Id: "e_\x001", Data: "x"},
		{Event: "msg\ndata: injected", Data: "x"},
		{Event: "msg\r", Data: "x"},
	}
	for _, msg := range cases {
		var out, buf bytes.Buffer
		if err := WriteMessage(&out, msg, &buf); !errors.Is(err, ErrInvalidField) {
			t.Errorf("WriteMessage(%q, %q) error = %v, want ErrInvalidField", msg.Id, msg.Event, err)
		}
		if out.Len() != 0 {
			t.Errorf("WriteMessage(%q, %q) wrote %q, want nothing", msg.Id, msg.Event, out.String())
		}
	}
}

func TestPushRejectsLineBreaks(t *testing.T) {
	w := &recordingResponseWriter{}
	p, err := CreateHttpPusher(w)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if err := p.Push(&Message{Id: "1\nevent: x", Data: "y"}); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("Push error = %v, want ErrInvalidField", err)
	}
	if err := p.Push(&Message{Id: "2", Data: "ok"}); err != nil {
		t.Fatalf("Push after rejected message: %v", err)
	}
	if out, _ := w.snapshot(); out != "id: 2\ndata: ok\n\n" {
		t.Fatalf("output = %q", out)
	}
}

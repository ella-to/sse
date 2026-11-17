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

// Browsers do not dispatch an event whose data buffer is empty unless a data
// line is present, so non-comment messages always carry one (SSE-B8).
func TestWriteMessageEmptyData(t *testing.T) {
	cases := []struct {
		msg  *Message
		want string
	}{
		{&Message{Event: "done"}, "event: done\ndata:\n\n"},
		{&Message{Id: "7"}, "id: 7\ndata:\n\n"},
		{&Message{}, "\n"}, // empty comment stays a bare separator
		{&Message{Data: "ping"}, ": ping\n\n"},
	}
	for _, c := range cases {
		var out, buf bytes.Buffer
		if err := WriteMessage(&out, c.msg, &buf); err != nil {
			t.Fatal(err)
		}
		if out.String() != c.want {
			t.Errorf("WriteMessage(%+v) = %q, want %q", *c.msg, out.String(), c.want)
		}

		if c.msg.Event == "" && c.msg.Id == "" {
			continue
		}
		got, err := ReadMessage(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if *got != *c.msg {
			t.Errorf("round trip = %+v, want %+v", *got, *c.msg)
		}
	}
}

func TestAppendMessageMatchesWriteMessage(t *testing.T) {
	big := bytes.Repeat([]byte("line\n"), 400)
	cases := []*Message{
		{Id: "1", Event: "msg", Data: "hello"},
		{Data: "comment\nsecond"},
		{Event: "x", Data: "a\n\nb\n"},
		{Id: "e_1", Data: string(big)},
		{},
	}
	for _, m := range cases {
		var out, buf bytes.Buffer
		if err := WriteMessage(&out, m, &buf); err != nil {
			t.Fatal(err)
		}
		prefix := []byte("keep")
		got, err := AppendMessage(prefix, []byte(m.Id), m.Event, []byte(m.Data))
		if err != nil {
			t.Fatal(err)
		}
		if string(got[:4]) != "keep" || string(got[4:]) != out.String() {
			t.Errorf("AppendMessage(%q) = %q, want %q", m.Id, got[4:], out.String())
		}
	}

	if got, err := AppendMessage([]byte("x"), []byte("a\nb"), "", nil); !errors.Is(err, ErrInvalidField) || string(got) != "x" {
		t.Errorf("invalid id: got %q, %v", got, err)
	}
}

func TestPushBytes(t *testing.T) {
	w := &recordingResponseWriter{}
	p, err := CreateHttpPusher(w)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	data := []byte(`{"a":1}`)
	if err := p.PushBytes([]byte("e_1"), "msg", data); err != nil {
		t.Fatal(err)
	}
	data[2] = 'Z' // the pusher must not retain the slice
	if out, _ := w.snapshot(); out != "id: e_1\nevent: msg\ndata: {\"a\":1}\n\n" {
		t.Fatalf("output = %q", out)
	}
}

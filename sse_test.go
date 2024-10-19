package sse_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"ella.to/sse"
)

func TestSSE(t *testing.T) {
	ctx := context.Background()

	w := httptest.NewRecorder()

	pusher, err := sse.CreatePusher(w, sse.WithHeader("Sample-Id", "123"))
	if err != nil {
		t.Fatal(err)
	}

	err = pusher.Push(ctx, "event", "data")
	if err != nil {
		t.Fatal(err)
	}

	err = pusher.Done(ctx)
	if err != nil {
		t.Fatal(err)
	}

	ch := sse.Receive(ctx, w.Result().Body)

	msg, ok := <-ch
	if !ok {
		t.Fatal("expected message, got closed channel")
	}

	if msg.Id != 1 {
		t.Fatalf("expected id 1, got %d", msg.Id)
	}

	if msg.Event != "event" {
		t.Fatalf("expected event event, got %s", msg.Event)
	}

	if string(msg.Data) != `data` {
		t.Fatalf(`expected data "data", got %s`, string(msg.Data))
	}

	msg = <-ch

	if msg.Id != 2 {
		t.Fatalf("expected id 2, got %d", msg.Id)
	}

	if msg.Event != "done" {
		t.Fatalf("expected event done, got %s", msg.Event)
	}

	if string(msg.Data) != `{}` {
		t.Fatalf(`expected data {}, got %s`, string(msg.Data))
	}
}

func TestWriteEvent(t *testing.T) {
	var sb strings.Builder

	sample := struct {
		Name string
	}{
		Name: "hello",
	}

	err := sse.WriteEvent(&sb, 1, "event", sample)
	if err != nil {
		t.Fatal(err)
	}

	err = sse.WriteEvent(&sb, 1, "event", sample)
	if err != nil {
		t.Fatal(err)
	}

	result := sb.String()

	if result != "id: 1\nevent: event\ndata: {\"Name\":\"hello\"}\n\nid: 1\nevent: event\ndata: {\"Name\":\"hello\"}\n\n" {
		t.Fatalf("expected %s, got %s", "id: 1\nevent: event\ndata: {\"Name\":\"hello\"}\n\nid: 1\nevent: event\ndata: {\"Name\":\"hello\"}\n\n", result)
	}
}

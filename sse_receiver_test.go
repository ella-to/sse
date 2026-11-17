package sse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReceiverContextCancelsInitialConnect(t *testing.T) {
	// the server never answers, so only the context can end the connect
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := CreateHttpReceiver(server.URL, WithHttpReceiverClient(server.Client()), WithHttpReceiverContext(ctx))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CreateHttpReceiver error = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("connect took %v after the context expired", d)
	}
}

func TestReceiverContextCancelsReceive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 1\ndata: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	r, err := CreateHttpReceiver(server.URL, WithHttpReceiverClient(server.Client()), WithHttpReceiverContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := r.Receive(); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := r.Receive(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive error = %v, want context.Canceled", err)
	}
}

func streamServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server
}

func TestReceiverReusesBuffersSafely(t *testing.T) {
	long := strings.Repeat("L", 100)
	body := "id: 1\ndata: " + long + "\ndata: second\n\n" + // multi-line, spilled
		"id: 2\ndata: short\n\n" + // single-line after a multi-line one
		"id: 3\nevent: e\ndata: a\ndata: b\n\n"
	server := streamServer(t, body)

	r, err := CreateHttpReceiver(server.URL, WithHttpReceiverClient(server.Client()), WithHttpReceiverBufferSize(16))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	want := []Message{
		{Id: "1", Data: long + "\nsecond"},
		{Id: "2", Data: "short"},
		{Id: "3", Event: "e", Data: "a\nb"},
	}
	var got []*Message
	for range want {
		m, err := r.Receive()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	for i := range want {
		if *got[i] != want[i] { // earlier messages must not change afterwards
			t.Errorf("message %d = %+v, want %+v", i, *got[i], want[i])
		}
	}
}

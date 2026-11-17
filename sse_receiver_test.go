package sse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

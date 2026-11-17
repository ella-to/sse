package sse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestReceiverMaxMessageSize(t *testing.T) {
	big := strings.Repeat("B", 4096)
	for _, body := range []string{
		"id: 1\ndata: " + big + "\n\nid: 2\ndata: ok\n\n",              // one long line
		"id: 1\n" + strings.Repeat("data: "+big[:100]+"\n", 50) + "\n", // many lines
	} {
		server := streamServer(t, body)
		r, err := CreateHttpReceiver(server.URL,
			WithHttpReceiverClient(server.Client()),
			WithHttpReceiverBufferSize(64),
			WithHttpReceiverMaxMessageSize(1024),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Receive(); !errors.Is(err, ErrMessageTooLarge) {
			t.Fatalf("Receive error = %v, want ErrMessageTooLarge", err)
		}
		r.Close()
	}

	// within the limit
	server := streamServer(t, "id: 1\ndata: "+big[:500]+"\n\n")
	r, err := CreateHttpReceiver(server.URL, WithHttpReceiverClient(server.Client()), WithHttpReceiverMaxMessageSize(1024))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if m, err := r.Receive(); err != nil || len(m.Data) != 500 {
		t.Fatalf("Receive = %v, %v", m, err)
	}
}

func TestReceiveIntoOverwritesFields(t *testing.T) {
	server := streamServer(t, "id: 1\nevent: a\ndata: x\n\nid: 2\ndata: y\n\nevent: a\ndata: z\n\n")
	r, err := CreateHttpReceiver(server.URL, WithHttpReceiverClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var m Message
	for _, want := range []Message{
		{Id: "1", Event: "a", Data: "x"},
		{Id: "2", Data: "y"},
		{Event: "a", Data: "z"},
	} {
		if err := r.ReceiveInto(&m); err != nil {
			t.Fatal(err)
		}
		if m != want {
			t.Fatalf("got %+v, want %+v", m, want)
		}
	}
}

func TestReceiverDoesNotRetryPermanentStatus(t *testing.T) {
	for _, tc := range []struct {
		code  int
		tries int32
	}{
		{http.StatusUnauthorized, 1},
		{http.StatusNotFound, 1},
		{http.StatusNoContent, 1},
		{http.StatusTooManyRequests, 3},
		{http.StatusServiceUnavailable, 3},
	} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(tc.code)
		}))
		_, err := CreateHttpReceiver(server.URL, WithHttpReceiverClient(server.Client()), WithHttpReceiverRetry(3, time.Millisecond))
		server.Close()

		var se *StatusError
		if !errors.As(err, &se) || se.Code != tc.code {
			t.Fatalf("status %d: error = %v", tc.code, err)
		}
		if got := calls.Load(); got != tc.tries {
			t.Errorf("status %d: %d requests, want %d", tc.code, got, tc.tries)
		}
	}
}

func TestReceiverStopsAfterNoContent(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "id: 1\ndata: last\n\n")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	r, err := CreateHttpReceiver(server.URL, WithHttpReceiverClient(server.Client()), WithHttpReceiverRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Receive(); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		var se *StatusError
		if _, err := r.Receive(); !errors.As(err, &se) || se.Code != http.StatusNoContent {
			t.Fatalf("Receive error = %v, want 204 StatusError", err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("%d requests, want 2 (no reconnect after 204)", got)
	}
}

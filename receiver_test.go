package sse

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHttpReceiver_Receive(t *testing.T) {
	// Create a test server that sends SSE events
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check headers
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Expected Accept header to be text/event-stream, got %s", r.Header.Get("Accept"))
		}
		if r.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("Expected Cache-Control header to be no-cache, got %s", r.Header.Get("Cache-Control"))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support flushing")
		}

		// Send a few test messages
		messages := []string{
			"id: 1\nevent: test\ndata: hello\n\n",
			"id: 2\nevent: test\ndata: world\n\n",
			"id: 3\nevent: done\ndata: finished\n\n",
		}

		for _, msg := range messages {
			fmt.Fprint(w, msg)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond) // Small delay between messages
		}
	}))
	defer server.Close()

	// Create httpReceiver
	receiver, err := NewHttpReceiver(server.URL)
	if err != nil {
		t.Fatalf("Failed to create httpReceiver: %v", err)
	}

	ctx := context.Background()

	// Receive first message
	msg1, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatalf("Failed to receive first message: %v", err)
	}
	if msg1.Id != "1" || msg1.Event != "test" || msg1.Data != "hello" {
		t.Errorf("Unexpected first message: id=%s, event=%s, data=%s", msg1.Id, msg1.Event, msg1.Data)
	}

	// Receive second message
	msg2, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatalf("Failed to receive second message: %v", err)
	}
	if msg2.Id != "2" || msg2.Event != "test" || msg2.Data != "world" {
		t.Errorf("Unexpected second message: id=%s, event=%s, data=%s", msg2.Id, msg2.Event, msg2.Data)
	}

	// Receive third message
	msg3, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatalf("Failed to receive third message: %v", err)
	}
	if msg3.Id != "3" || msg3.Event != "done" || msg3.Data != "finished" {
		t.Errorf("Unexpected third message: id=%s, event=%s, data=%s", msg3.Id, msg3.Event, msg3.Data)
	}

	// Try to receive another message (should get EOF or connection closed)
	_, err = receiver.Receive(ctx)
	if err == nil {
		t.Error("Expected error when receiving after server closed connection")
	}
}

func TestHttpReceiver_ContextCancellation(t *testing.T) {
	// Create a server that sends messages slowly
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support flushing")
		}

		// Send one message quickly, then wait
		fmt.Fprint(w, "id: 1\nevent: test\ndata: quick\n\n")
		flusher.Flush()

		// Then wait for context cancellation
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, "id: 2\nevent: test\ndata: slow\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	receiver, err := NewHttpReceiver(server.URL)
	if err != nil {
		t.Fatalf("Failed to create httpReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Receive first message (should succeed)
	msg1, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatalf("Failed to receive first message: %v", err)
	}
	if msg1.Id != "1" || msg1.Data != "quick" {
		t.Errorf("Unexpected message: id=%s, data=%s", msg1.Id, msg1.Data)
	}

	// Try to receive second message (should fail due to context timeout)
	start := time.Now()
	_, err = receiver.Receive(ctx)
	duration := time.Since(start)

	if err == nil {
		t.Error("Expected error due to context timeout")
	}

	// Should fail quickly due to context timeout, not wait the full 2 seconds
	if duration > 200*time.Millisecond {
		t.Errorf("Context cancellation took too long: %v", duration)
	}
}

func TestHttpReceiver_ServerError(t *testing.T) {
	// Create a server that returns an error status
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "Internal Server Error")
	}))
	defer server.Close()

	// Create receiver with minimal retries to speed up the test
	receiver, err := NewHttpReceiver(
		server.URL,
		WithMaxRetries(1),
		WithInitialDelay(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("Failed to create httpReceiver: %v", err)
	}

	ctx := context.Background()

	// Should fail to receive due to server error after retries
	_, err = receiver.Receive(ctx)
	if err == nil {
		t.Error("Expected error due to server error status")
	}

	// Should contain "max retries exceeded" since the retry transport handles the retries
	expectedError := "max retries exceeded"
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("Expected error to contain '%s', got: %v", expectedError, err)
	}
}

func TestHttpReceiver_NonRetryableError(t *testing.T) {
	// Create a server that returns a 404 (non-retryable)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "Not Found")
	}))
	defer server.Close()

	receiver, err := NewHttpReceiver(server.URL)
	if err != nil {
		t.Fatalf("Failed to create httpReceiver: %v", err)
	}

	ctx := context.Background()

	// Should fail immediately due to 404 (no retries)
	_, err = receiver.Receive(ctx)
	if err == nil {
		t.Error("Expected error due to 404 status")
	}

	expectedError := "unexpected status code: 404"
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("Expected error to contain '%s', got: %v", expectedError, err)
	}
}

func TestHttpReceiver_InvalidURL(t *testing.T) {
	receiver, err := NewHttpReceiver("invalid-url")
	if err != nil {
		t.Fatalf("Failed to create httpReceiver: %v", err)
	}

	ctx := context.Background()

	// Should fail to receive due to invalid URL
	_, err = receiver.Receive(ctx)
	if err == nil {
		t.Error("Expected error due to invalid URL")
	}
}

func TestHttpReceiver_ConcurrentReceive(t *testing.T) {
	// Create a server that sends multiple messages
	messageCount := 10
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support flushing")
		}

		for i := 0; i < messageCount; i++ {
			fmt.Fprintf(w, "id: %d\nevent: test\ndata: message%d\n\n", i, i)
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer server.Close()

	receiver, err := NewHttpReceiver(server.URL)
	if err != nil {
		t.Fatalf("Failed to create httpReceiver: %v", err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	messages := make([]*Message, messageCount)
	errors := make([]error, messageCount)

	// Start multiple goroutines trying to receive messages
	// This tests for race conditions in the connection management
	for i := 0; i < messageCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			msg, err := receiver.Receive(ctx)
			messages[index] = msg
			errors[index] = err
		}(i)
		// Small delay to stagger the requests
		time.Sleep(time.Millisecond)
	}

	wg.Wait()

	// Check that all messages were received successfully
	receivedMessages := 0
	for i := 0; i < messageCount; i++ {
		if errors[i] == nil && messages[i] != nil {
			receivedMessages++
		}
	}

	// We should receive all messages (though order might be different due to concurrency)
	if receivedMessages != messageCount {
		t.Errorf("Expected to receive %d messages, got %d", messageCount, receivedMessages)
	}

	// Check for any unexpected errors
	for i, err := range errors {
		if err != nil && err != io.EOF {
			t.Errorf("Unexpected error at index %d: %v", i, err)
		}
	}
}

func TestHttpReceiver_Reconnection(t *testing.T) {
	connectionCount := 0
	var mu sync.Mutex

	// Create a server that tracks connection count
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connectionCount++
		currentConnection := connectionCount
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support flushing")
		}

		// Send one message then close connection to simulate network issues
		fmt.Fprintf(w, "id: %d\nevent: test\ndata: connection%d\n\n", currentConnection, currentConnection)
		flusher.Flush()

		// Close connection immediately to force reconnection
	}))
	defer server.Close()

	receiver, err := NewHttpReceiver(server.URL)
	if err != nil {
		t.Fatalf("Failed to create httpReceiver: %v", err)
	}

	ctx := context.Background()

	// First receive - should establish first connection
	msg1, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatalf("Failed to receive first message: %v", err)
	}
	if msg1.Data != "connection1" {
		t.Errorf("Expected first message to be from connection1, got: %s", msg1.Data)
	}

	// Second receive - connection should be closed, so new connection needed
	// This will fail because the connection was closed, and the receiver needs to handle reconnection
	_, err = receiver.Receive(ctx)
	if err == nil {
		t.Error("Expected error due to closed connection")
	}

	// Third receive - should establish new connection
	msg3, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatalf("Failed to receive after reconnection: %v", err)
	}
	if msg3.Data != "connection2" {
		t.Errorf("Expected message from connection2, got: %s", msg3.Data)
	}

	mu.Lock()
	finalConnectionCount := connectionCount
	mu.Unlock()

	// Should have made at least 2 connections
	if finalConnectionCount < 2 {
		t.Errorf("Expected at least 2 connections, got %d", finalConnectionCount)
	}
}

func TestHttpReceiver_WithRetryOptions(t *testing.T) {
	attempts := 0
	var mu sync.Mutex

	// Create a server that fails first few attempts
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		currentAttempt := attempts
		mu.Unlock()

		// Fail first 2 attempts
		if currentAttempt <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Success on 3rd attempt
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		fmt.Fprint(w, "id: 1\nevent: test\ndata: success\n\n")
	}))
	defer server.Close()

	// Create receiver with retry options
	receiver, err := NewHttpReceiver(
		server.URL,
		WithMaxRetries(3),
		WithInitialDelay(10*time.Millisecond),
		WithMaxDelay(100*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("Failed to create httpReceiver: %v", err)
	}

	ctx := context.Background()

	// Should succeed after retries
	start := time.Now()
	msg, err := receiver.Receive(ctx)
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("Failed to receive message after retries: %v", err)
	}

	if msg.Data != "success" {
		t.Errorf("Expected success message, got: %s", msg.Data)
	}

	mu.Lock()
	finalAttempts := attempts
	mu.Unlock()

	// Should have made 3 attempts
	if finalAttempts != 3 {
		t.Errorf("Expected 3 attempts, got %d", finalAttempts)
	}

	// Should have taken some time due to retries
	if duration < 10*time.Millisecond {
		t.Errorf("Expected retry delays, but request completed too quickly: %v", duration)
	}
}

package sse

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"
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

//
// httpReceiver
//

type httpReceiver struct {
	url       string
	client    *http.Client
	receiver  Receiver
	connected bool
	// Connection retry configuration
	maxConnectionRetries int
	initialRetryDelay    time.Duration
	maxRetryDelay        time.Duration
	// Mutex to protect concurrent access to receiver and connected fields
	mu sync.RWMutex
}

var _ Receiver = (*httpReceiver)(nil)

func (hr *httpReceiver) Receive(ctx context.Context) (*Message, error) {
	// Retry connection establishment if needed
	for attempt := 0; attempt <= hr.maxConnectionRetries; attempt++ {
		// Check connection status with read lock
		hr.mu.RLock()
		connected := hr.connected
		receiver := hr.receiver
		hr.mu.RUnlock()

		// If not connected or receiver is nil, establish connection
		if !connected || receiver == nil {
			if err := hr.connect(ctx); err != nil {
				// If this is the last attempt, return the error
				if attempt == hr.maxConnectionRetries {
					return nil, fmt.Errorf("failed to establish connection after %d attempts: %w", hr.maxConnectionRetries+1, err)
				}

				// Calculate backoff delay
				delay := hr.calculateConnectionBackoff(attempt)

				// Wait with context cancellation support
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
					continue // Try again
				}
			}
			// Re-read receiver after successful connection
			hr.mu.RLock()
			receiver = hr.receiver
			hr.mu.RUnlock()
		}

		// Try to receive a message
		msg, err := receiver.Receive(ctx)
		if err != nil {
			// Connection lost, reset state with write lock
			hr.mu.Lock()
			hr.connected = false
			hr.receiver = nil
			hr.mu.Unlock()

			// If this is the last attempt, return the error
			if attempt == hr.maxConnectionRetries {
				return nil, fmt.Errorf("failed to receive message after %d connection attempts: %w", hr.maxConnectionRetries+1, err)
			}

			// Continue to retry connection
			continue
		}

		return msg, nil
	}

	// This should never be reached due to the loop structure
	return nil, fmt.Errorf("unexpected error in connection retry logic")
}

func (hr *httpReceiver) connect(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", hr.url, nil)
	if err != nil {
		return err
	}

	// Set SSE headers
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := hr.client.Do(req)
	if err != nil {
		return err
	}

	// Check if response is valid
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Create receiver from response body and update state with write lock
	hr.mu.Lock()
	hr.receiver = NewReceiver(resp.Body)
	hr.connected = true
	hr.mu.Unlock()

	return nil
}

// calculateConnectionBackoff calculates exponential backoff with max delay for connection retries
func (hr *httpReceiver) calculateConnectionBackoff(attempt int) time.Duration {
	delay := time.Duration(float64(hr.initialRetryDelay) * math.Pow(2, float64(attempt)))
	if delay > hr.maxRetryDelay {
		delay = hr.maxRetryDelay
	}
	return delay
}

// Connection retry options for httpReceiver
type httpReceiverOpt func(*httpReceiver) error

func WithConnectionMaxRetries(maxRetries int) httpReceiverOpt {
	return func(hr *httpReceiver) error {
		if maxRetries < 0 {
			return fmt.Errorf("maxRetries must be non-negative")
		}
		hr.maxConnectionRetries = maxRetries
		return nil
	}
}

func WithConnectionInitialDelay(delay time.Duration) httpReceiverOpt {
	return func(hr *httpReceiver) error {
		if delay <= 0 {
			return fmt.Errorf("initial delay must be positive")
		}
		hr.initialRetryDelay = delay
		return nil
	}
}

func WithConnectionMaxDelay(delay time.Duration) httpReceiverOpt {
	return func(hr *httpReceiver) error {
		if delay <= 0 {
			return fmt.Errorf("max delay must be positive")
		}
		hr.maxRetryDelay = delay
		return nil
	}
}

func NewHttpReceiver(url string, opts ...interface{}) (*httpReceiver, error) {
	// Separate retry transport options from connection retry options
	var retryTransportOpts []retryTransportOpt
	var httpReceiverOpts []httpReceiverOpt

	for _, opt := range opts {
		switch o := opt.(type) {
		case retryTransportOpt:
			retryTransportOpts = append(retryTransportOpts, o)
		case httpReceiverOpt:
			httpReceiverOpts = append(httpReceiverOpts, o)
		default:
			return nil, fmt.Errorf("unsupported option type: %T", opt)
		}
	}

	client, err := NewRetryClient(retryTransportOpts...)
	if err != nil {
		return nil, err
	}

	hr := &httpReceiver{
		url:    url,
		client: client,
		// Default connection retry configuration
		maxConnectionRetries: 3,
		initialRetryDelay:    500 * time.Millisecond,
		maxRetryDelay:        30 * time.Second,
	}

	// Apply connection retry options
	for _, opt := range httpReceiverOpts {
		if err := opt(hr); err != nil {
			return nil, err
		}
	}

	return hr, nil
}

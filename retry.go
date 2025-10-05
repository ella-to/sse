package sse

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// RetryTransport wraps an http.RoundTripper to add headers, retries, and exponential backoff
type retryTransport struct {
	Transport    http.RoundTripper
	MaxRetries   int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	Headers      map[string]string
}

type retryTransportOpt func(*retryTransport) error

func WithMaxRetries(maxRetries int) retryTransportOpt {
	return func(t *retryTransport) error {
		if maxRetries < 0 {
			return fmt.Errorf("maxRetries must be non-negative")
		}
		t.MaxRetries = maxRetries
		return nil
	}
}

func WithInitialDelay(delay time.Duration) retryTransportOpt {
	return func(t *retryTransport) error {
		if delay <= 0 {
			return fmt.Errorf("initial delay must be positive")
		}
		t.InitialDelay = delay
		return nil
	}
}

func WithMaxDelay(delay time.Duration) retryTransportOpt {
	return func(t *retryTransport) error {
		if delay <= 0 {
			return fmt.Errorf("max delay must be positive")
		}
		t.MaxDelay = delay
		return nil
	}
}

func WithHeaders(headers map[string]string) retryTransportOpt {
	return func(t *retryTransport) error {
		if headers == nil {
			return fmt.Errorf("headers cannot be nil")
		}
		t.Headers = headers
		return nil
	}
}

// RoundTrip implements the http.RoundTripper interface
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Inject headers into every request
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}

	var resp *http.Response
	var err error

	for attempt := 0; attempt <= t.MaxRetries; attempt++ {
		// Clone the request body if it exists (for retries)
		var bodyClone io.ReadCloser
		if req.Body != nil && req.GetBody != nil {
			bodyClone, err = req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("failed to clone request body: %w", err)
			}
			req.Body = bodyClone
		}

		// Make the actual request
		resp, err = t.Transport.RoundTrip(req)

		// If successful or non-retryable, return
		if err == nil && !shouldRetry(resp.StatusCode) {
			return resp, nil
		}

		// Close response body if it exists
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}

		// Don't sleep after the last attempt
		if attempt < t.MaxRetries {
			delay := t.calculateBackoff(attempt)
			fmt.Printf("Attempt %d failed, retrying in %v...\n", attempt+1, delay)
			time.Sleep(delay)
		}
	}

	return resp, fmt.Errorf("max retries exceeded: %w", err)
}

// calculateBackoff calculates exponential backoff with max delay
func (t *retryTransport) calculateBackoff(attempt int) time.Duration {
	delay := time.Duration(float64(t.InitialDelay) * math.Pow(2, float64(attempt)))
	if delay > t.MaxDelay {
		delay = t.MaxDelay
	}
	return delay
}

// shouldRetry determines if a status code should trigger a retry
func shouldRetry(statusCode int) bool {
	// Retry on 5xx server errors and 429 Too Many Requests
	return statusCode >= 500 || statusCode == 429
}

// NewRetryClient creates an HTTP client with retry logic and header injection
func NewRetryClient(opts ...retryTransportOpt) (*http.Client, error) {
	transport := &retryTransport{
		Transport:    http.DefaultTransport,
		MaxRetries:   3,
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
		Headers:      make(map[string]string),
	}

	for _, opt := range opts {
		if err := opt(transport); err != nil {
			return nil, err
		}
	}

	return &http.Client{
		Transport: transport,
	}, nil
}

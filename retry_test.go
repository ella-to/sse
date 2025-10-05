package sse

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewRetryClient(t *testing.T) {
	tests := []struct {
		name        string
		opts        []retryTransportOpt
		expectError bool
		checkFunc   func(*http.Client) error
	}{
		{
			name:        "default client",
			opts:        nil,
			expectError: false,
			checkFunc: func(client *http.Client) error {
				transport := client.Transport.(*retryTransport)
				if transport.MaxRetries != 3 {
					return fmt.Errorf("expected MaxRetries 3, got %d", transport.MaxRetries)
				}
				if transport.InitialDelay != 1*time.Second {
					return fmt.Errorf("expected InitialDelay 1s, got %v", transport.InitialDelay)
				}
				if transport.MaxDelay != 30*time.Second {
					return fmt.Errorf("expected MaxDelay 30s, got %v", transport.MaxDelay)
				}
				return nil
			},
		},
		{
			name: "custom options",
			opts: []retryTransportOpt{
				WithMaxRetries(5),
				WithInitialDelay(2 * time.Second),
				WithMaxDelay(60 * time.Second),
				WithHeaders(map[string]string{"User-Agent": "test-client"}),
			},
			expectError: false,
			checkFunc: func(client *http.Client) error {
				transport := client.Transport.(*retryTransport)
				if transport.MaxRetries != 5 {
					return fmt.Errorf("expected MaxRetries 5, got %d", transport.MaxRetries)
				}
				if transport.InitialDelay != 2*time.Second {
					return fmt.Errorf("expected InitialDelay 2s, got %v", transport.InitialDelay)
				}
				if transport.MaxDelay != 60*time.Second {
					return fmt.Errorf("expected MaxDelay 60s, got %v", transport.MaxDelay)
				}
				if transport.Headers["User-Agent"] != "test-client" {
					return fmt.Errorf("expected User-Agent header, got %v", transport.Headers)
				}
				return nil
			},
		},
		{
			name: "invalid max retries",
			opts: []retryTransportOpt{
				WithMaxRetries(-1),
			},
			expectError: true,
		},
		{
			name: "invalid initial delay",
			opts: []retryTransportOpt{
				WithInitialDelay(-1 * time.Second),
			},
			expectError: true,
		},
		{
			name: "invalid max delay",
			opts: []retryTransportOpt{
				WithMaxDelay(-1 * time.Second),
			},
			expectError: true,
		},
		{
			name: "nil headers",
			opts: []retryTransportOpt{
				WithHeaders(nil),
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewRetryClient(tt.opts...)

			if tt.expectError && err == nil {
				t.Error("Expected error but got none")
			}

			if !tt.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if !tt.expectError && tt.checkFunc != nil {
				if err := tt.checkFunc(client); err != nil {
					t.Errorf("Client validation failed: %v", err)
				}
			}
		})
	}
}

func TestRetryTransportHeaderInjection(t *testing.T) {
	// Create a test server that echoes headers
	requestCount := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)

		// Echo the headers in response
		for name, values := range r.Header {
			for _, value := range values {
				w.Header().Add("Echo-"+name, value)
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer server.Close()

	headers := map[string]string{
		"User-Agent":    "test-client/1.0",
		"Authorization": "Bearer test-token",
		"Custom-Header": "custom-value",
	}

	client, err := NewRetryClient(
		WithMaxRetries(0), // No retries for this test
		WithHeaders(headers),
	)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	// Verify headers were injected
	for key, expectedValue := range headers {
		echoHeader := "Echo-" + key
		if resp.Header.Get(echoHeader) != expectedValue {
			t.Errorf("Header %s not injected correctly. Expected %s, got %s",
				key, expectedValue, resp.Header.Get(echoHeader))
		}
	}

	// Verify only one request was made
	if atomic.LoadInt32(&requestCount) != 1 {
		t.Errorf("Expected 1 request, got %d", atomic.LoadInt32(&requestCount))
	}
}

func TestRetryTransportRetryLogic(t *testing.T) {
	tests := []struct {
		name               string
		serverResponses    []int // Status codes to return in sequence
		expectedRequests   int   // Total requests expected
		maxRetries         int
		expectFinalSuccess bool
	}{
		{
			name:               "success on first try",
			serverResponses:    []int{200},
			expectedRequests:   1,
			maxRetries:         3,
			expectFinalSuccess: true,
		},
		{
			name:               "success after retries",
			serverResponses:    []int{500, 502, 200},
			expectedRequests:   3,
			maxRetries:         3,
			expectFinalSuccess: true,
		},
		{
			name:               "max retries exceeded",
			serverResponses:    []int{500, 500, 500, 500},
			expectedRequests:   4, // 1 initial + 3 retries
			maxRetries:         3,
			expectFinalSuccess: false,
		},
		{
			name:               "retry on 429",
			serverResponses:    []int{429, 200},
			expectedRequests:   2,
			maxRetries:         3,
			expectFinalSuccess: true,
		},
		{
			name:               "no retry on 4xx (except 429)",
			serverResponses:    []int{400},
			expectedRequests:   1,
			maxRetries:         3,
			expectFinalSuccess: false, // 400 is not successful
		},
		{
			name:               "no retry on 3xx",
			serverResponses:    []int{301},
			expectedRequests:   1,
			maxRetries:         3,
			expectFinalSuccess: true, // 3xx is not an error for our purposes
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestCount := int32(0)
			responseIndex := int32(0)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&requestCount, 1)
				index := atomic.AddInt32(&responseIndex, 1) - 1

				if int(index) < len(tt.serverResponses) {
					w.WriteHeader(tt.serverResponses[index])
				} else {
					// If we've exhausted our planned responses, return the last one
					w.WriteHeader(tt.serverResponses[len(tt.serverResponses)-1])
				}
				w.Write([]byte("response"))
			}))
			defer server.Close()

			client, err := NewRetryClient(
				WithMaxRetries(tt.maxRetries),
				WithInitialDelay(10*time.Millisecond), // Fast retries for testing
				WithMaxDelay(100*time.Millisecond),
			)
			if err != nil {
				t.Fatalf("Failed to create client: %v", err)
			}

			resp, err := client.Get(server.URL)

			// Check request count
			actualRequests := atomic.LoadInt32(&requestCount)
			if int(actualRequests) != tt.expectedRequests {
				t.Errorf("Expected %d requests, got %d", tt.expectedRequests, actualRequests)
			}

			// Check final result
			if tt.expectFinalSuccess {
				if err != nil {
					t.Errorf("Expected success but got error: %v", err)
				}
				if resp != nil {
					resp.Body.Close()
				}
			} else {
				// For non-success cases, we might still get a response (e.g., 400 status)
				// but we should check the status code
				if resp != nil {
					resp.Body.Close()
					finalStatus := tt.serverResponses[len(tt.serverResponses)-1]
					if finalStatus >= 400 && finalStatus != 429 && finalStatus < 500 {
						// 4xx errors (except 429) shouldn't be retried, so we get the response
						if resp.StatusCode != finalStatus {
							t.Errorf("Expected status %d, got %d", finalStatus, resp.StatusCode)
						}
					}
				}
			}
		})
	}
}

func TestRetryTransportExponentialBackoff(t *testing.T) {
	transport := &retryTransport{
		Transport:    http.DefaultTransport,
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     1 * time.Second,
	}

	tests := []struct {
		attempt     int
		expectedMin time.Duration
		expectedMax time.Duration
	}{
		{0, 100 * time.Millisecond, 100 * time.Millisecond},
		{1, 200 * time.Millisecond, 200 * time.Millisecond},
		{2, 400 * time.Millisecond, 400 * time.Millisecond},
		{3, 800 * time.Millisecond, 800 * time.Millisecond},
		{4, 1 * time.Second, 1 * time.Second}, // Capped at MaxDelay
		{5, 1 * time.Second, 1 * time.Second}, // Still capped
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tt.attempt), func(t *testing.T) {
			delay := transport.calculateBackoff(tt.attempt)

			if delay < tt.expectedMin || delay > tt.expectedMax {
				t.Errorf("Attempt %d: expected delay between %v and %v, got %v",
					tt.attempt, tt.expectedMin, tt.expectedMax, delay)
			}
		})
	}
}

func TestShouldRetry(t *testing.T) {
	tests := []struct {
		statusCode  int
		shouldRetry bool
	}{
		// Success codes - no retry
		{200, false},
		{201, false},
		{204, false},
		{299, false},

		// Redirect codes - no retry
		{300, false},
		{301, false},
		{302, false},
		{399, false},

		// Client error codes - no retry (except 429)
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{429, true}, // Too Many Requests - should retry
		{499, false},

		// Server error codes - retry
		{500, true},
		{501, true},
		{502, true},
		{503, true},
		{504, true},
		{599, true},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("status_%d", tt.statusCode), func(t *testing.T) {
			result := shouldRetry(tt.statusCode)
			if result != tt.shouldRetry {
				t.Errorf("shouldRetry(%d) = %v, want %v", tt.statusCode, result, tt.shouldRetry)
			}
		})
	}
}

func TestRetryTransportWithRequestBody(t *testing.T) {
	requestCount := int32(0)
	var receivedBodies []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)

		body, _ := io.ReadAll(r.Body)
		receivedBodies = append(receivedBodies, string(body))

		// Fail first two requests, succeed on third
		if count <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client, err := NewRetryClient(
		WithMaxRetries(3),
		WithInitialDelay(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	requestBody := "test request body"
	resp, err := client.Post(server.URL, "text/plain", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should have made 3 requests (1 initial + 2 retries)
	if atomic.LoadInt32(&requestCount) != 3 {
		t.Errorf("Expected 3 requests, got %d", atomic.LoadInt32(&requestCount))
	}

	// All requests should have received the same body
	for i, body := range receivedBodies {
		if body != requestBody {
			t.Errorf("Request %d: expected body %q, got %q", i+1, requestBody, body)
		}
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected final status 200, got %d", resp.StatusCode)
	}
}

func TestRetryTransportTimeout(t *testing.T) {
	// Create a server that responds slowly
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewRetryClient(
		WithMaxRetries(1),
		WithInitialDelay(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Set a short timeout
	client.Timeout = 50 * time.Millisecond

	_, err = client.Get(server.URL)
	if err == nil {
		t.Error("Expected timeout error but got none")
	}

	// Should be a timeout error, not a retry error
	if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("Expected timeout error, got: %v", err)
	}
}

func TestRetryTransportRealWorldScenario(t *testing.T) {
	// Simulate a real-world scenario with temporary service issues
	requestCount := int32(0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)

		// Simulate different failure modes
		switch count {
		case 1:
			w.WriteHeader(http.StatusBadGateway) // 502 - should retry
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable) // 503 - should retry
		case 3:
			// Success
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status": "success"}`))
		}
	}))
	defer server.Close()

	client, err := NewRetryClient(
		WithMaxRetries(3),
		WithInitialDelay(50*time.Millisecond),
		WithMaxDelay(500*time.Millisecond),
		WithHeaders(map[string]string{
			"User-Agent": "httputil-test/1.0",
			"Accept":     "application/json",
		}),
	)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	start := time.Now()
	resp, err := client.Get(server.URL)
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should have made 3 requests total
	if atomic.LoadInt32(&requestCount) != 3 {
		t.Errorf("Expected 3 requests, got %d", atomic.LoadInt32(&requestCount))
	}

	// Final response should be successful
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Should have taken at least the retry delays (50ms + 100ms = 150ms minimum)
	if duration < 100*time.Millisecond {
		t.Errorf("Request completed too quickly: %v", duration)
	}

	// Read and verify response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	expectedBody := `{"status": "success"}`
	if string(body) != expectedBody {
		t.Errorf("Expected body %q, got %q", expectedBody, string(body))
	}
}

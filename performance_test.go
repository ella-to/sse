package sse_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ella.to/sse"
)

// Performance comparison tests
func BenchmarkThroughputComparison(b *testing.B) {
	b.Run("SmallMessages", func(b *testing.B) {
		benchmarkThroughput(b, "small", "small data", 100)
	})

	b.Run("MediumMessages", func(b *testing.B) {
		mediumData := strings.Repeat("x", 500)
		benchmarkThroughput(b, "medium", mediumData, 100)
	})

	b.Run("LargeMessages", func(b *testing.B) {
		largeData := strings.Repeat("x", 2000)
		benchmarkThroughput(b, "large", largeData, 50)
	})
}

func benchmarkThroughput(b *testing.B, msgType, data string, numMessages int) {
	b.ReportAllocs()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pusher, err := sse.NewHttpPusher(w, 0)
		if err != nil {
			return
		}
		defer pusher.Close()

		for i := range numMessages {
			msg := sse.NewMessage(strconv.Itoa(i), msgType, data)
			if err := pusher.Push(msg); err != nil {
				break
			}
		}
	}))
	defer server.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		client := http.Client{Timeout: 30 * time.Second}
		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}

		receiver := sse.NewReceiver(resp.Body)
		for j := 0; j < numMessages; j++ {
			_, err := receiver.Receive(context.Background())
			if err != nil {
				break
			}
		}
		resp.Body.Close()
	}
}

// Memory usage under load test
func BenchmarkMemoryEfficiency(b *testing.B) {
	b.ReportAllocs()

	const numConcurrentClients = 50
	const messagesPerClient = 100

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pusher, err := sse.NewHttpPusher(w, 0)
		if err != nil {
			return
		}
		defer pusher.Close()

		for i := range messagesPerClient {
			msg := sse.NewMessage(strconv.Itoa(i), "memory", "data_"+strconv.Itoa(i))
			if err := pusher.Push(msg); err != nil {
				break
			}
		}
	}))
	defer server.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		var totalProcessed int64

		wg.Add(numConcurrentClients)

		for j := 0; j < numConcurrentClients; j++ {
			go func() {
				defer wg.Done()

				client := http.Client{Timeout: 30 * time.Second}
				req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
				resp, err := client.Do(req)
				if err != nil {
					return
				}
				defer resp.Body.Close()

				receiver := sse.NewReceiver(resp.Body)
				processed := int64(0)
				for k := 0; k < messagesPerClient; k++ {
					_, err := receiver.Receive(context.Background())
					if err != nil {
						break
					}
					processed++
				}
				atomic.AddInt64(&totalProcessed, processed)
			}()
		}

		wg.Wait()

		expected := int64(numConcurrentClients * messagesPerClient)
		if totalProcessed != expected {
			b.Logf("Processed %d/%d messages", totalProcessed, expected)
		}
	}
}

// Test message reuse for efficiency
func BenchmarkMessageReuse(b *testing.B) {
	b.ReportAllocs()

	msg := &sse.Message{}
	buffer := make([]byte, 512)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msg.SetMessage(strconv.Itoa(i), "reuse", "data_"+strconv.Itoa(i))

		for {
			n, err := msg.Read(buffer)
			if err != nil {
				break
			}
			if n == 0 {
				break
			}
		}
	}
}

// Benchmark parsing different message sizes
func BenchmarkParsingEfficiency(b *testing.B) {
	testCases := []struct {
		name string
		data string
	}{
		{"Tiny", "id: 1\ndata: x\n\n"},
		{"Small", "id: 12345\nevent: test\ndata: " + strings.Repeat("x", 50) + "\n\n"},
		{"Medium", "id: 12345\nevent: test\ndata: " + strings.Repeat("x", 500) + "\n\n"},
		{"Large", "id: 12345\nevent: test\ndata: " + strings.Repeat("x", 2000) + "\n\n"},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := strings.NewReader(tc.data)
				ch := sse.Parse(r)
				<-ch
			}
		})
	}
}

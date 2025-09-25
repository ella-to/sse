package sse_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ella.to/sse"
)

func TestParse(t *testing.T) {
	r := strings.NewReader("data: hello\n\n")
	ch := sse.Parse(r)

	msg := <-ch
	if msg.Data != "hello" {
		t.Error("Data mismatch")
	}
	if msg.Id != "" {
		t.Error("Id mismatch")
	}
	if msg.Event != "" {
		t.Error("Event mismatch")
	}
}

func TestParseMultipleMessages(t *testing.T) {
	data := `id: 1
event: test
data: message 1

id: 2
event: test
data: message 2

`
	r := strings.NewReader(data)
	ch := sse.Parse(r)

	// First message
	msg1 := <-ch
	if msg1.Id != "1" || msg1.Event != "test" || msg1.Data != "message 1" {
		t.Errorf("First message mismatch: %+v", msg1)
	}

	// Second message
	msg2 := <-ch
	if msg2.Id != "2" || msg2.Event != "test" || msg2.Data != "message 2" {
		t.Errorf("Second message mismatch: %+v", msg2)
	}
}

func TestParseWithComments(t *testing.T) {
	data := `: this is a comment
id: 1
: another comment
data: test data

`
	r := strings.NewReader(data)
	ch := sse.Parse(r)

	msg := <-ch
	if msg.Id != "1" || msg.Data != "test data" {
		t.Errorf("Message with comments failed: %+v", msg)
	}
}

func TestParseLarge(t *testing.T) {
	file, err := os.Open("./testdata/test01.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	ch := sse.Parse(file)
	count := 0

	for msg := range ch {
		count++
		if testing.Verbose() {
			fmt.Printf("%s\n", msg)
		}
	}

	if count == 0 {
		t.Error("No messages parsed from test file")
	}
}

func TestPushReceive(t *testing.T) {
	n := 10

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pusher, err := sse.NewHttpPusher(w, 500*time.Millisecond)
		if err != nil {
			t.Error(err)
			return
		}
		defer pusher.Close()

		for i := range n {
			msg := sse.NewMessage("id_"+strconv.Itoa(i), "event", "data_"+strconv.Itoa(i))
			err = pusher.Push(msg)
			if err != nil {
				break
			}
		}
	}))
	defer server.Close()

	client := http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Error(err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Error(err)
		return
	}
	defer resp.Body.Close()

	r := sse.NewReceiver(resp.Body)
	count := 0

	for {
		msg, err := r.Receive(context.Background())
		if err != nil {
			break
		}
		count++
		if testing.Verbose() {
			fmt.Println(msg)
		}
	}

	if count == 0 {
		t.Error("No messages received")
	}
}

func TestPusherReceiver(t *testing.T) {
	n := 10000 // Reduced for faster testing
	c := 5     // Reduced concurrent connections

	var wg sync.WaitGroup
	var totalReceived int64

	wg.Add(c)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pusher, err := sse.NewHttpPusher(w, 10*time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer pusher.Close()

		for i := range n {
			msg := sse.NewMessage("id_"+strconv.Itoa(i), "event", "data_"+strconv.Itoa(i))
			err = pusher.Push(msg)
			if err != nil {
				break
			}
		}
	}))
	defer server.Close()

	client := http.Client{Timeout: 30 * time.Second}

	for range c {
		go func() {
			defer wg.Done()

			req, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				t.Error(err)
				return
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()

			r := sse.NewReceiver(resp.Body)
			received := int64(0)

			for {
				_, err := r.Receive(context.Background())
				if err != nil {
					break
				}
				received++
			}

			atomic.AddInt64(&totalReceived, received)
		}()
	}

	wg.Wait()

	if totalReceived == 0 {
		t.Error("No messages received in stress test")
	}

	t.Logf("Total messages received: %d", totalReceived)
}

// Benchmark tests for performance measurement

func BenchmarkPushReceive(b *testing.B) {
	b.ReportAllocs()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pusher, err := sse.NewHttpPusher(w, 0) // No timeout for benchmarking
		if err != nil {
			b.Error(err)
			return
		}
		defer pusher.Close()

		for i := 0; i < b.N; i++ {
			msg := sse.NewMessage("id_"+strconv.Itoa(i), "benchmark", "data_"+strconv.Itoa(i))
			if err := pusher.Push(msg); err != nil {
				break
			}
		}
	}))
	defer server.Close()

	client := http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)

	b.ResetTimer()
	resp, err := client.Do(req)
	if err != nil {
		b.Fatal(err)
	}
	defer resp.Body.Close()

	receiver := sse.NewReceiver(resp.Body)
	for i := 0; i < b.N; i++ {
		_, err := receiver.Receive(context.Background())
		if err != nil {
			break
		}
	}
}

func BenchmarkParseMessages(b *testing.B) {
	b.ReportAllocs()

	// Create test data
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString(fmt.Sprintf("id: %d\nevent: test\ndata: message %d\n\n", i, i))
	}
	testData := sb.String()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := strings.NewReader(testData)
		ch := sse.Parse(r)

		// Consume all messages
		for range ch {
		}
	}
}

func BenchmarkParseConcurrent(b *testing.B) {
	b.ReportAllocs()

	// Create test data
	testData := "id: 1\nevent: test\ndata: concurrent test\n\n"

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r := strings.NewReader(testData)
			ch := sse.Parse(r)

			// Consume the message
			<-ch
		}
	})
}

func BenchmarkHighThroughput(b *testing.B) {
	b.ReportAllocs()

	const numMessages = 1000
	const numConcurrentClients = 10

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pusher, err := sse.NewHttpPusher(w, 0)
		if err != nil {
			return
		}
		defer pusher.Close()

		for i := 0; i < numMessages; i++ {
			msg := sse.NewMessage("id_"+strconv.Itoa(i), "throughput", "data_"+strconv.Itoa(i))
			if err := pusher.Push(msg); err != nil {
				break
			}
		}
	}))
	defer server.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
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
				for k := 0; k < numMessages; k++ {
					_, err := receiver.Receive(context.Background())
					if err != nil {
						break
					}
				}
			}()
		}

		wg.Wait()
	}
}

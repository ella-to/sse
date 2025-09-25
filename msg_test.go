package sse_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"ella.to/sse"
)

func TestReadWrite(t *testing.T) {
	msg := sse.NewMessage("1", "event", "data")

	var buffer bytes.Buffer

	_, err := io.Copy(&buffer, msg)
	if err != nil {
		t.Fatal(err)
	}

	var recv sse.Message

	_, err = io.Copy(&recv, &buffer)
	if err != nil {
		t.Fatal(err)
	}

	if recv.Id != "1" {
		t.Error("Id mismatch")
	}

	if recv.Event != "event" {
		t.Error("Event mismatch")
	}

	if recv.Data != "data" {
		t.Error("Data mismatch")
	}
}

func TestMessagePooling(t *testing.T) {
	// Test buffer pooling by creating many messages
	messages := make([]*sse.Message, 1000)
	for i := range messages {
		messages[i] = sse.NewMessage("1", "test", "data")
	}

	// Read all messages to trigger buffer pooling
	buffer := make([]byte, 512)
	for _, msg := range messages {
		for {
			n, err := msg.Read(buffer)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
	}

	// No specific assertion needed - this tests that pooling doesn't break
}

func BenchmarkMsgReader(b *testing.B) {
	b.ReportAllocs()
	buffer := make([]byte, 512)

	var msg *sse.Message

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Reset message for reuse
		msg = sse.NewMessage("1", "event", "data")
		for {
			n, err := msg.Read(buffer)
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
	}
}

func BenchmarkMsgWriter(b *testing.B) {
	b.ReportAllocs()

	testData := []byte(`id: 1
event: event
data: data

`)
	var msg sse.Message

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := msg.Write(testData)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMsgReaderLargeData(b *testing.B) {
	b.ReportAllocs()

	largeData := strings.Repeat("x", 1024) // 1KB of data
	buffer := make([]byte, 2048)

	var msg *sse.Message

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Reset message for reuse
		msg = sse.NewMessage("large-id-12345", "large-event-name", largeData)
		for {
			n, err := msg.Read(buffer)
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
	}
}

func BenchmarkMsgConcurrent(b *testing.B) {
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		buffer := make([]byte, 512)
		for pb.Next() {
			msg := sse.NewMessage("1", "event", "data")
			for {
				n, err := msg.Read(buffer)
				if err == io.EOF {
					break
				}
				if err != nil {
					b.Fatal(err)
				}
				if n == 0 {
					break
				}
			}
		}
	})
}

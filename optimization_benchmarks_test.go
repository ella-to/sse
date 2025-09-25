package sse_test

import (
	"strconv"
	"strings"
	"testing"

	"ella.to/sse"
)

// Benchmark the new batch parser for high throughput scenarios
func BenchmarkBatchParsing(b *testing.B) {
	// Create a large multi-message input
	var builder strings.Builder
	for i := 0; i < 100; i++ {
		builder.WriteString("id: ")
		builder.WriteString(strconv.Itoa(i))
		builder.WriteString("\nevent: test\ndata: message_")
		builder.WriteString(strconv.Itoa(i))
		builder.WriteString("\n\n")
	}
	data := builder.String()

	b.ReportAllocs()
	b.ResetTimer()
	
	for i := 0; i < b.N; i++ {
		r := strings.NewReader(data)
		ch := sse.BatchParse(r, 10)
		for batch := range ch {
			for _, msg := range batch {
				sse.PutMessage(msg)
			}
		}
	}
}

// Benchmark message pool effectiveness
func BenchmarkMessagePoolEfficiency(b *testing.B) {
	b.ReportAllocs()
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Simulate typical usage pattern
		msg := sse.GetMessage()
		msg.SetMessage(strconv.Itoa(i), "test", "data")
		
		// Read the message
		buf := make([]byte, 256)
		for {
			_, err := msg.Read(buf)
			if err != nil {
				break
			}
		}
		
		// Return to pool
		sse.PutMessage(msg)
	}
}

// Benchmark comparing pooled vs non-pooled message creation
func BenchmarkMessageCreationComparison(b *testing.B) {
	b.Run("WithPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			msg := sse.GetMessage()
			msg.SetMessage("123", "event", "data")
			sse.PutMessage(msg)
		}
	})
	
	b.Run("WithoutPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			msg := &sse.Message{}
			msg.SetMessage("123", "event", "data")
			// No pooling, just let GC handle it
		}
	})
}

// Benchmark the optimized parsing methods
func BenchmarkParseMethods(b *testing.B) {
	data := "id: 12345\nevent: test\ndata: " + strings.Repeat("x", 200) + "\n\n"
	
	b.Run("OriginalParse", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			r := strings.NewReader(data)
			ch := sse.Parse(r)
			msg := <-ch
			sse.PutMessage(msg)
		}
	})
	
	b.Run("FastParse", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			r := strings.NewReader(data)
			ch := sse.FastParse(r)
			msg := <-ch
			sse.PutMessage(msg)
		}
	})
}
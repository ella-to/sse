package sse_test

import (
	"bytes"
	"io"
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

func BenchmarkMsgReder(b *testing.B) {
	b.ReportAllocs()

	msg := sse.NewMessage("1", "event", "data")

	buffer := [512]byte{}

	for i := 0; i < b.N; i++ {
		msg.Read(buffer[:])
	}
}

func BenchmarkMsgWriter(b *testing.B) {
	b.ReportAllocs()

	testData := []byte(`id: 1
event: event
data: data

`)
	var msg sse.Message

	for i := 0; i < b.N; i++ {
		_, err := msg.Write(testData)
		if err != nil {
			b.Fatal(err)
		}
	}
}

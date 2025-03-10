package exp_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"ella.to/sse/exp"
)

func TestReadWrite(t *testing.T) {
	msg := exp.NewMessage("1", "event", "data")

	var buffer bytes.Buffer

	_, err := io.Copy(&buffer, msg)
	if err != nil {
		t.Fatal(err)
	}

	var recv exp.Message

	_, err = io.Copy(&recv, &buffer)
	if err != nil {
		t.Fatal(err)
	}

	fmt.Println(recv.Id)
	fmt.Println(recv.Event)
	fmt.Println(recv.Data)
}

func BenchmarkMsgReder(b *testing.B) {
	b.ReportAllocs()

	msg := exp.NewMessage("1", "event", "data")

	var buffer bytes.Buffer

	_, err := io.Copy(&buffer, msg)
	if err != nil {
		b.Fatal(err)
	}

	var recv exp.Message

	for i := 0; i < b.N; i++ {
		buffer.Reset()
		_, err = io.Copy(&recv, &buffer)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMsgWriter(b *testing.B) {
	b.ReportAllocs()

	msg := exp.NewMessage("1", "event", "data")

	var buffer bytes.Buffer

	for i := 0; i < b.N; i++ {
		buffer.Reset()
		_, err := io.Copy(&buffer, msg)
		if err != nil {
			b.Fatal(err)
		}
	}
}

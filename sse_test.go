package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type closeTrackingResponseWriter struct {
	header http.Header
	buf    bytes.Buffer
	closed atomic.Bool
}

func (w *closeTrackingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *closeTrackingResponseWriter) Write(b []byte) (int, error) {
	if w.closed.Load() {
		return 0, http.ErrServerClosed
	}
	return w.buf.Write(b)
}

func (w *closeTrackingResponseWriter) WriteHeader(statusCode int) {}

func (w *closeTrackingResponseWriter) Flush() {}

func (w *closeTrackingResponseWriter) Close() error {
	w.closed.Store(true)
	return nil
}

type closeTrackingReadCloser struct {
	closed atomic.Bool
}

func (rc *closeTrackingReadCloser) Read(_ []byte) (int, error) {
	return 0, io.EOF
}

func (rc *closeTrackingReadCloser) Close() error {
	rc.closed.Store(true)
	return nil
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

var (
	benchReadSink  *Message
	benchWriteSink int
)

func TestReadMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    *Message
		wantErr error
	}{
		{
			name:  "parses id event and multiline data",
			input: "id: 42\nevent: update\ndata: line1\ndata: line2\n\n",
			want:  &Message{Id: "42", Event: "update", Data: "line1\nline2"},
		},
		{
			name:  "parses comment as data",
			input: ": heartbeat\n\n",
			want:  &Message{Data: "heartbeat"},
		},
		{
			name:  "skips leading blanks",
			input: "\n\n data: ignored\nid: 9\ndata: ok\n\n",
			want:  &Message{Id: "9", Data: "ok"},
		},
		{
			name:  "returns partial message on eof",
			input: "event: end\ndata: last",
			want:  &Message{Event: "end", Data: "last"},
		},
		{
			name:    "returns eof with no message",
			input:   "",
			wantErr: io.EOF,
		},
		{
			name:    "unknown fields are ignored",
			input:   "retry: 1000\n\n",
			wantErr: io.EOF,
		},
		{
			name:  "invalid id containing nul is ignored",
			input: "id: valid\nid: bad\x00id\ndata: payload\n\n",
			want:  &Message{Id: "valid", Data: "payload"},
		},
		{
			name:  "supports crlf line endings",
			input: "id: 7\r\nevent: ping\r\ndata: ok\r\n\r\n",
			want:  &Message{Id: "7", Event: "ping", Data: "ok"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ReadMessage(strings.NewReader(tt.input))
			if tt.wantErr != nil {
				if err != tt.wantErr {
					t.Fatalf("ReadMessage() error = %v, want %v", err, tt.wantErr)
				}
				if got != nil {
					t.Fatalf("ReadMessage() = %#v, want nil", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("ReadMessage() unexpected error = %v", err)
			}
			if got == nil {
				t.Fatal("ReadMessage() returned nil message")
			}
			if *got != *tt.want {
				t.Fatalf("ReadMessage() = %#v, want %#v", *got, *tt.want)
			}
		})
	}
}

func TestReadMessageLongLine(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("x", 32*1024)
	input := "id: 1\ndata: " + payload + "\n\n"
	br := bufio.NewReaderSize(strings.NewReader(input), 64)

	msg, err := ReadMessage(br)
	if err != nil {
		t.Fatalf("ReadMessage() unexpected error = %v", err)
	}
	if msg == nil {
		t.Fatal("ReadMessage() returned nil message")
	}
	if msg.Id != "1" {
		t.Fatalf("Id = %q, want %q", msg.Id, "1")
	}
	if msg.Data != payload {
		t.Fatalf("Data length = %d, want %d", len(msg.Data), len(payload))
	}
}

func TestWriteMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  *Message
		want string
	}{
		{
			name: "writes full message",
			msg:  &Message{Id: "42", Event: "update", Data: "hello"},
			want: "id: 42\nevent: update\ndata: hello\n\n",
		},
		{
			name: "writes multiline data",
			msg:  &Message{Id: "1", Data: "line1\nline2\nline3"},
			want: "id: 1\ndata: line1\ndata: line2\ndata: line3\n\n",
		},
		{
			name: "writes comment message",
			msg:  NewComment("keepalive\nsecond"),
			want: ": keepalive\n: second\n\n",
		},
		{
			name: "writes blank message",
			msg:  &Message{},
			want: "\n",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			var scratch bytes.Buffer
			err := WriteMessage(&out, tt.msg, &scratch)
			if err != nil {
				t.Fatalf("WriteMessage() error = %v", err)
			}
			if out.String() != tt.want {
				t.Fatalf("WriteMessage() = %q, want %q", out.String(), tt.want)
			}
		})
	}
}

func TestHttpReceiverReceiveMultipleMessages(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 1\ndata: first\n\nid: 2\ndata: second\n\n")
	}))
	defer server.Close()

	receiver, err := CreateHttpReceiver(
		server.URL,
		WithHttpReceiverClient(server.Client()),
		WithHttpReceiverRetry(1, 0),
	)
	if err != nil {
		t.Fatalf("CreateHttpReceiver() error = %v", err)
	}
	defer func() {
		_ = receiver.Close()
	}()

	msg1, err := receiver.Receive()
	if err != nil {
		t.Fatalf("first Receive() error = %v", err)
	}
	if msg1.Id != "1" || msg1.Data != "first" {
		t.Fatalf("first Receive() = %#v, want id=1 data=first", msg1)
	}

	msg2, err := receiver.Receive()
	if err != nil {
		t.Fatalf("second Receive() error = %v", err)
	}
	if msg2.Id != "2" || msg2.Data != "second" {
		t.Fatalf("second Receive() = %#v, want id=2 data=second", msg2)
	}
}

func TestHttpReceiverReconnectSendsLastEventID(t *testing.T) {
	t.Parallel()

	var connCount atomic.Int32
	lastEventIDSeen := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		n := connCount.Add(1)
		switch n {
		case 1:
			_, _ = io.WriteString(w, "id: 1\ndata: first\n\n")
		case 2:
			lastEventIDSeen <- req.Header.Get("Last-Event-ID")
			_, _ = io.WriteString(w, "id: 2\ndata: second\n\n")
		default:
			http.Error(w, "unexpected connection", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	receiver, err := CreateHttpReceiver(
		server.URL,
		WithHttpReceiverClient(server.Client()),
		WithHttpReceiverRetry(3, time.Millisecond),
	)
	if err != nil {
		t.Fatalf("CreateHttpReceiver() error = %v", err)
	}
	defer func() {
		_ = receiver.Close()
	}()

	msg1, err := receiver.Receive()
	if err != nil {
		t.Fatalf("first Receive() error = %v", err)
	}
	if msg1.Id != "1" || msg1.Data != "first" {
		t.Fatalf("first Receive() = %#v, want id=1 data=first", msg1)
	}

	msg2, err := receiver.Receive()
	if err != nil {
		t.Fatalf("second Receive() error = %v", err)
	}
	if msg2.Id != "2" || msg2.Data != "second" {
		t.Fatalf("second Receive() = %#v, want id=2 data=second", msg2)
	}

	select {
	case got := <-lastEventIDSeen:
		if got != "1" {
			t.Fatalf("Last-Event-ID = %q, want %q", got, "1")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnect header")
	}
}

func TestHttpReceiverCloseUnblocksReceive(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		pusher, err := CreateHttpPusher(w)
		if err != nil {
			return
		}
		defer pusher.Close()

		<-req.Context().Done()
	}))
	defer server.Close()

	receiver, err := CreateHttpReceiver(
		server.URL,
		WithHttpReceiverClient(server.Client()),
		WithHttpReceiverRetry(1, 0),
	)
	if err != nil {
		t.Fatalf("CreateHttpReceiver() error = %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, recvErr := receiver.Receive()
		errCh <- recvErr
	}()

	time.Sleep(50 * time.Millisecond)

	if err := receiver.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Receive() error = %v, want %v", err, http.ErrServerClosed)
		}
	case <-time.After(time.Second):
		t.Fatal("Receive() did not unblock after Close()")
	}
}

func TestHttpPusherCloseClosesUnderlyingWriter(t *testing.T) {
	t.Parallel()

	w := &closeTrackingResponseWriter{}
	pusher, err := CreateHttpPusher(w)
	if err != nil {
		t.Fatalf("CreateHttpPusher() error = %v", err)
	}

	if err := pusher.Push(&Message{Data: "hello"}); err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	if err := pusher.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !w.closed.Load() {
		t.Fatal("Close() did not close underlying writer")
	}

	if err := pusher.Push(&Message{Data: "again"}); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Push() after Close() error = %v, want %v", err, http.ErrServerClosed)
	}
	if err := pusher.Close(); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("second Close() error = %v, want %v", err, http.ErrServerClosed)
	}
}

func TestHttpReceiverCloseClosesUnderlyingBody(t *testing.T) {
	t.Parallel()

	body := &closeTrackingReadCloser{}
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
			}, nil
		}),
	}

	receiver, err := CreateHttpReceiver("http://example.com/sse", WithHttpReceiverClient(client))
	if err != nil {
		t.Fatalf("CreateHttpReceiver() error = %v", err)
	}

	if err := receiver.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !body.closed.Load() {
		t.Fatal("Close() did not close underlying body")
	}

	if err := receiver.Close(); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("second Close() error = %v, want %v", err, http.ErrServerClosed)
	}
}

func TestHttpReceiverReceiveReturnsErrorWhenReconnectFails(t *testing.T) {
	t.Parallel()

	var connCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if connCount.Add(1) == 1 {
			_, _ = io.WriteString(w, "id: 1\ndata: first\n\n")
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))

	client := server.Client()
	url := server.URL

	receiver, err := CreateHttpReceiver(
		url,
		WithHttpReceiverClient(client),
		WithHttpReceiverRetry(2, 10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("CreateHttpReceiver() error = %v", err)
	}

	msg, err := receiver.Receive()
	if err != nil {
		t.Fatalf("first Receive() error = %v", err)
	}
	if msg.Id != "1" || msg.Data != "first" {
		t.Fatalf("first Receive() = %#v, want id=1 data=first", msg)
	}

	server.Close()

	_, err = receiver.Receive()
	if err == nil {
		t.Fatal("second Receive() expected reconnect failure, got nil")
	}
	if !strings.Contains(err.Error(), "failed to connect") {
		t.Fatalf("second Receive() error = %v, want failed to connect", err)
	}

	_ = receiver.Close()
}

func TestThroughputMessagesPerSecond(t *testing.T) {
	pr, pw := io.Pipe()

	const runFor = 300 * time.Millisecond
	deadline := time.Now().Add(runFor)

	var sent atomic.Int64
	var received atomic.Int64

	writerErrCh := make(chan error, 1)
	readerErrCh := make(chan error, 1)

	go func() {
		var scratch bytes.Buffer
		for time.Now().Before(deadline) {
			id := strconv.FormatInt(sent.Load()+1, 10)
			msg := &Message{Id: id, Event: "throughput", Data: "payload"}
			scratch.Reset()
			if err := WriteMessage(pw, msg, &scratch); err != nil {
				writerErrCh <- err
				return
			}
			sent.Add(1)
		}

		writerErrCh <- pw.Close()
	}()

	go func() {
		br := bufio.NewReaderSize(pr, 4096)
		for {
			msg, err := ReadMessage(br)
			if err == io.EOF {
				readerErrCh <- nil
				return
			}
			if err != nil {
				readerErrCh <- err
				return
			}
			if msg != nil {
				received.Add(1)
			}
		}
	}()

	start := time.Now()
	if err := <-writerErrCh; err != nil {
		t.Fatalf("writer error: %v", err)
	}
	if err := <-readerErrCh; err != nil {
		t.Fatalf("reader error: %v", err)
	}
	elapsed := time.Since(start)

	sentCount := sent.Load()
	receivedCount := received.Load()
	if sentCount == 0 {
		t.Fatal("sent message count is zero")
	}
	if receivedCount != sentCount {
		t.Fatalf("received %d messages, sent %d", receivedCount, sentCount)
	}

	sentRate := float64(sentCount) / elapsed.Seconds()
	receivedRate := float64(receivedCount) / elapsed.Seconds()
	t.Logf("throughput: sent=%d (%.2f msg/s), received=%d (%.2f msg/s)", sentCount, sentRate, receivedCount, receivedRate)
}

func TestThroughputOverHTTPMessagesPerSecond(t *testing.T) {
	const runFor = 300 * time.Millisecond

	var sent atomic.Int64
	var received atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		pusher, err := CreateHttpPusher(w)
		if err != nil {
			return
		}
		defer pusher.Close()

		for {
			select {
			case <-req.Context().Done():
				return
			default:
			}

			id := strconv.FormatInt(sent.Load()+1, 10)
			if err := pusher.Push(&Message{Id: id, Event: "throughput-http", Data: "payload"}); err != nil {
				return
			}
			sent.Add(1)
		}
	}))
	defer server.Close()

	receiver, err := CreateHttpReceiver(
		server.URL,
		WithHttpReceiverClient(server.Client()),
		WithHttpReceiverRetry(1, 0),
	)
	if err != nil {
		t.Fatalf("CreateHttpReceiver() error = %v", err)
	}
	defer func() {
		_ = receiver.Close()
	}()

	start := time.Now()
	deadline := start.Add(runFor)
	for time.Now().Before(deadline) {
		msg, err := receiver.Receive()
		if err != nil {
			t.Fatalf("Receive() error = %v", err)
		}
		if msg != nil {
			received.Add(1)
		}
	}
	elapsed := time.Since(start)

	sentCount := sent.Load()
	receivedCount := received.Load()
	if sentCount == 0 || receivedCount == 0 {
		t.Fatalf("invalid throughput counters: sent=%d received=%d", sentCount, receivedCount)
	}
	if receivedCount > sentCount {
		t.Fatalf("received %d messages but only %d sent", receivedCount, sentCount)
	}

	sentRate := float64(sentCount) / elapsed.Seconds()
	receivedRate := float64(receivedCount) / elapsed.Seconds()
	t.Logf("http throughput: sent=%d (%.2f msg/s), received=%d (%.2f msg/s)", sentCount, sentRate, receivedCount, receivedRate)
}

func BenchmarkReadMessageSmall(b *testing.B) {
	input := "id: 42\nevent: tick\ndata: hello\n\n"
	br := bufio.NewReaderSize(strings.NewReader(""), 4096)
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()

	for b.Loop() {
		br.Reset(strings.NewReader(input))
		msg, err := ReadMessage(br)
		if err != nil {
			b.Fatalf("ReadMessage() error = %v", err)
		}
		benchReadSink = msg
	}
}

func BenchmarkReadMessageLargeData(b *testing.B) {
	payload := strings.Repeat("a", 64*1024)
	input := "id: 1\nevent: bulk\ndata: " + payload + "\n\n"
	br := bufio.NewReaderSize(strings.NewReader(""), 256)
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()

	for b.Loop() {
		br.Reset(strings.NewReader(input))
		msg, err := ReadMessage(br)
		if err != nil {
			b.Fatalf("ReadMessage() error = %v", err)
		}
		benchReadSink = msg
	}
}

func BenchmarkWriteMessageSmall(b *testing.B) {
	msg := &Message{Id: "42", Event: "tick", Data: "hello"}
	var out bytes.Buffer
	var scratch bytes.Buffer
	b.ReportAllocs()
	b.SetBytes(int64(len(msg.Id) + len(msg.Event) + len(msg.Data) + 20))
	b.ResetTimer()

	for b.Loop() {
		out.Reset()
		scratch.Reset()
		if err := WriteMessage(&out, msg, &scratch); err != nil {
			b.Fatalf("WriteMessage() error = %v", err)
		}
		benchWriteSink += out.Len()
	}
}

func BenchmarkWriteMessageLargeData(b *testing.B) {
	msg := &Message{Id: "1", Event: "bulk", Data: strings.Repeat("line-data", 8192)}
	var out bytes.Buffer
	var scratch bytes.Buffer
	b.ReportAllocs()
	b.SetBytes(int64(len(msg.Id) + len(msg.Event) + len(msg.Data) + 20))
	b.ResetTimer()

	for b.Loop() {
		out.Reset()
		scratch.Reset()
		if err := WriteMessage(&out, msg, &scratch); err != nil {
			b.Fatalf("WriteMessage() error = %v", err)
		}
		benchWriteSink += out.Len()
	}
}

func BenchmarkHttpReceiverReceiveSteadyState(b *testing.B) {
	var messageID atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "flusher not supported", http.StatusInternalServerError)
			return
		}

		for {
			select {
			case <-req.Context().Done():
				return
			default:
			}

			id := strconv.FormatInt(messageID.Add(1), 10)
			if _, err := io.WriteString(w, "id: "+id+"\ndata: benchmark\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	defer server.Close()

	receiver, err := CreateHttpReceiver(
		server.URL,
		WithHttpReceiverClient(server.Client()),
		WithHttpReceiverRetry(1, 0),
	)
	if err != nil {
		b.Fatalf("CreateHttpReceiver() error = %v", err)
	}
	defer func() {
		_ = receiver.Close()
	}()

	b.ReportAllocs()
	b.SetBytes(int64(len("id: 1\ndata: benchmark\n\n")))
	b.ResetTimer()

	for b.Loop() {
		msg, err := receiver.Receive()
		if err != nil {
			b.Fatalf("Receive() error = %v", err)
		}
		benchReadSink = msg
	}
}

func BenchmarkHttpReceiverReceiveReconnect(b *testing.B) {
	var connCount atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		id := strconv.FormatInt(connCount.Add(1), 10)
		_, _ = io.WriteString(w, "id: "+id+"\ndata: reconnect\n\n")
	}))
	defer server.Close()

	receiver, err := CreateHttpReceiver(
		server.URL,
		WithHttpReceiverClient(server.Client()),
		WithHttpReceiverRetry(3, 0),
	)
	if err != nil {
		b.Fatalf("CreateHttpReceiver() error = %v", err)
	}
	defer func() {
		_ = receiver.Close()
	}()

	b.ReportAllocs()
	b.SetBytes(int64(len("id: 1\ndata: reconnect\n\n")))
	b.ResetTimer()

	for b.Loop() {
		msg, err := receiver.Receive()
		if err != nil {
			b.Fatalf("Receive() error = %v", err)
		}
		benchReadSink = msg
	}
}

func BenchmarkThroughputEndToEnd(b *testing.B) {
	pr, pw := io.Pipe()
	reader := bufio.NewReaderSize(pr, 4096)

	writerErrCh := make(chan error, 1)
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()

	go func() {
		var scratch bytes.Buffer
		for i := 0; i < b.N; i++ {
			msg := &Message{Id: strconv.Itoa(i + 1), Event: "throughput", Data: "payload"}
			scratch.Reset()
			if err := WriteMessage(pw, msg, &scratch); err != nil {
				writerErrCh <- err
				return
			}
		}
		writerErrCh <- pw.Close()
	}()

	received := 0
	for received < b.N {
		msg, err := ReadMessage(reader)
		if err != nil {
			b.Fatalf("ReadMessage() error = %v", err)
		}
		if msg != nil {
			received++
			benchReadSink = msg
		}
	}

	if err := <-writerErrCh; err != nil {
		b.Fatalf("writer error: %v", err)
	}

	elapsed := time.Since(start)
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "sent-msg/s")
	b.ReportMetric(float64(received)/elapsed.Seconds(), "recv-msg/s")
}

func BenchmarkThroughputOverHTTP(b *testing.B) {
	var sent atomic.Int64
	target := int64(b.N)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		pusher, err := CreateHttpPusher(w)
		if err != nil {
			return
		}
		defer pusher.Close()

		for i := int64(0); i < target; i++ {
			id := strconv.FormatInt(i+1, 10)
			if err := pusher.Push(&Message{Id: id, Event: "throughput-http", Data: "payload"}); err != nil {
				return
			}
			sent.Add(1)
		}
	}))
	defer server.Close()

	receiver, err := CreateHttpReceiver(
		server.URL,
		WithHttpReceiverClient(server.Client()),
		WithHttpReceiverRetry(1, 0),
	)
	if err != nil {
		b.Fatalf("CreateHttpReceiver() error = %v", err)
	}
	defer func() {
		_ = receiver.Close()
	}()

	b.ReportAllocs()
	b.SetBytes(int64(len("id: 1\nevent: throughput-http\ndata: payload\n\n")))
	b.ResetTimer()
	start := time.Now()

	received := 0
	for received < b.N {
		msg, err := receiver.Receive()
		if err != nil {
			b.Fatalf("Receive() error = %v", err)
		}
		if msg != nil {
			received++
			benchReadSink = msg
		}
	}

	elapsed := time.Since(start)
	b.ReportMetric(float64(sent.Load())/elapsed.Seconds(), "sent-msg/s")
	b.ReportMetric(float64(received)/elapsed.Seconds(), "recv-msg/s")
}

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
		pusher, err := CreateHttpPusher(w)
		if err != nil {
			return
		}
		defer pusher.Close()

		for i := range numMessages {
			if err := pusher.Push(&Message{
				Id:    strconv.Itoa(i),
				Event: msgType,
				Data:  data,
			}); err != nil {
				break
			}
		}
	}))
	defer server.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		receiver, err := CreateHttpReceiver(server.URL)
		if err != nil {
			b.Fatalf("CreateHttpReceiver() error = %v", err)
		}

		for j := 0; j < numMessages; j++ {
			_, err := receiver.Receive()
			if err != nil {
				break
			}
		}

		receiver.Close()
	}
}

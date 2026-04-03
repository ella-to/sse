package sse

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

type Message struct {
	Id    string
	Event string
	Data  string
}

func ReadMessage(r io.Reader) (*Message, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 4096)
	}

	trimLineEnd := func(b []byte) []byte {
		if n := len(b); n > 0 && b[n-1] == '\n' {
			b = b[:n-1]
			if n = len(b); n > 0 && b[n-1] == '\r' {
				b = b[:n-1]
			}
		}
		return b
	}

	var lineBuf bytes.Buffer
	readLine := func() ([]byte, error) {
		line, err := br.ReadSlice('\n')
		if err == nil {
			return trimLineEnd(line), nil
		}

		if err != bufio.ErrBufferFull {
			if err == io.EOF && len(line) > 0 {
				return trimLineEnd(line), io.EOF
			}
			return nil, err
		}

		lineBuf.Reset()
		lineBuf.Write(line)
		for {
			line, err = br.ReadSlice('\n')
			lineBuf.Write(line)
			if err != bufio.ErrBufferFull {
				break
			}
		}

		if err != nil && err != io.EOF {
			return nil, err
		}

		return trimLineEnd(lineBuf.Bytes()), err
	}

	var msg Message
	var dataBuf bytes.Buffer
	haveMessage := false
	haveData := false

	for {
		line, err := readLine()
		if err != nil && err != io.EOF {
			return nil, err
		}

		if len(line) == 0 {
			if haveMessage {
				if haveData {
					msg.Data = dataBuf.String()
				}
				return &msg, nil
			}

			if err == io.EOF {
				return nil, io.EOF
			}

			continue
		}

		if line[0] == ':' {
			haveMessage = true
			comment := line[1:]
			if len(comment) > 0 && comment[0] == ' ' {
				comment = comment[1:]
			}
			if haveData {
				dataBuf.WriteByte('\n')
			}
			dataBuf.Write(comment)
			haveData = true
		} else {
			sep := bytes.IndexByte(line, ':')
			field := line
			value := []byte{}
			if sep >= 0 {
				field = line[:sep]
				value = line[sep+1:]
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
			}

			switch {
			case bytes.Equal(field, []byte("data")):
				haveMessage = true
				if haveData {
					dataBuf.WriteByte('\n')
				}
				dataBuf.Write(value)
				haveData = true
			case bytes.Equal(field, []byte("id")):
				haveMessage = true
				if bytes.IndexByte(value, 0) < 0 {
					msg.Id = string(value)
				}
			case bytes.Equal(field, []byte("event")):
				haveMessage = true
				msg.Event = string(value)
			}
		}

		if err == io.EOF {
			if haveMessage {
				if haveData {
					msg.Data = dataBuf.String()
				}
				return &msg, nil
			}
			return nil, io.EOF
		}
	}
}

func WriteMessage(w io.Writer, msg *Message, buf *bytes.Buffer) error {
	isComment := true

	if msg.Id != "" {
		buf.WriteString("id: ")
		buf.WriteString(msg.Id)
		buf.WriteByte('\n')
		isComment = false
	}

	if msg.Event != "" {
		buf.WriteString("event: ")
		buf.WriteString(msg.Event)
		buf.WriteByte('\n')
		isComment = false
	}

	if msg.Data != "" {
		start := 0
		for i := 0; i <= len(msg.Data); i++ {
			if i == len(msg.Data) || msg.Data[i] == '\n' {
				if isComment {
					buf.WriteString(": ")
				} else {
					buf.WriteString("data: ")
				}
				buf.WriteString(msg.Data[start:i])
				buf.WriteByte('\n')
				start = i + 1
			}
		}
	}

	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

func NewComment(data string) *Message {
	return &Message{
		Data: data,
	}
}

type Pusher interface {
	Push(msg *Message) error
	Close() error
}

type HttpPusher struct {
	w            http.ResponseWriter
	flusher      http.Flusher
	closer       io.Closer
	pingDuration time.Duration
	pingCancel   func()
	closed       atomic.Bool
	mux          sync.Mutex
	buffer       bytes.Buffer
}

var _ Pusher = (*HttpPusher)(nil)

func setTimeout(d time.Duration, timeoutFunc func()) func() {
	timer := time.AfterFunc(d, timeoutFunc)
	return func() {
		timer.Stop()
	}
}

func (p *HttpPusher) Push(msg *Message) error {
	if p.closed.Load() {
		return http.ErrServerClosed
	}

	p.mux.Lock()
	defer p.mux.Unlock()

	if p.closed.Load() {
		return http.ErrServerClosed
	}

	if p.pingDuration > 0 {
		if p.pingCancel != nil {
			p.pingCancel()
		}
		p.pingCancel = setTimeout(p.pingDuration, func() {
			_ = p.Push(NewComment("ping"))
		})
	}

	p.buffer.Reset()

	err := WriteMessage(p.w, msg, &p.buffer)
	if err != nil {
		return err
	}

	p.flusher.Flush()

	return nil
}

func (p *HttpPusher) Close() error {
	if p.closed.Swap(true) {
		return http.ErrServerClosed
	}

	p.mux.Lock()
	if p.pingCancel != nil {
		p.pingCancel()
		p.pingCancel = nil
	}
	closer := p.closer
	p.mux.Unlock()

	if closer != nil {
		_ = closer.Close()
	}

	return nil
}

type HttpPusherOption func(*HttpPusher)

func WithHttpPusherHeader(key, value string) HttpPusherOption {
	return func(p *HttpPusher) {
		p.w.Header().Set(key, value)
	}
}

func WithHttpPusherPingDuration(d time.Duration) HttpPusherOption {
	return func(p *HttpPusher) {
		p.pingDuration = d
	}
}

func CreateHttpPusher(w http.ResponseWriter, opts ...HttpPusherOption) (*HttpPusher, error) {
	out, ok := w.(http.Flusher)
	if !ok {
		return nil, http.ErrNotSupported
	}

	pusher := &HttpPusher{
		w:       w,
		flusher: out,
	}
	if closer, ok := w.(io.Closer); ok {
		pusher.closer = closer
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	for _, opt := range opts {
		opt(pusher)
	}

	out.Flush()

	return pusher, nil
}

type Receiver interface {
	Receive() (*Message, error)
	Close() error
}

type HttpReceiver struct {
	url        string
	client     *http.Client
	requestURL *url.URL
	baseHeader http.Header
	body       io.ReadCloser
	reader     *bufio.Reader
	retryMax   int
	retryDelay time.Duration
	respHeader func(header http.Header)

	lastEventID string
	closed      atomic.Bool
	closeOnce   sync.Once
	closeCh     chan struct{}
	mux         sync.Mutex
	recvMux     sync.Mutex
}

var _ Receiver = (*HttpReceiver)(nil)

func (r *HttpReceiver) Receive() (*Message, error) {
	r.recvMux.Lock()
	defer r.recvMux.Unlock()

	for {
		if r.closed.Load() {
			return nil, http.ErrServerClosed
		}

		reader, err := r.getReader()
		if err != nil {
			if r.closed.Load() {
				return nil, http.ErrServerClosed
			}
			return nil, err
		}

		msg, err := ReadMessage(reader)
		if err == nil {
			if msg != nil && msg.Id != "" {
				r.mux.Lock()
				r.lastEventID = msg.Id
				r.mux.Unlock()
			}
			return msg, nil
		}

		if r.closed.Load() {
			return nil, http.ErrServerClosed
		}

		r.closeBody()

		if err := r.connect(); err != nil {
			if r.closed.Load() {
				return nil, http.ErrServerClosed
			}
			return nil, err
		}
	}
}

func (r *HttpReceiver) Close() error {
	if r.closed.Swap(true) {
		return http.ErrServerClosed
	}

	r.closeOnce.Do(func() {
		close(r.closeCh)
	})

	r.closeBody()

	return nil
}

func (r *HttpReceiver) connect() error {
	attempts := r.retryMax
	if attempts <= 0 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if r.closed.Load() {
			return http.ErrServerClosed
		}

		u := *r.requestURL
		req := &http.Request{
			Method: http.MethodGet,
			URL:    &u,
			Header: r.baseHeader.Clone(),
		}

		r.mux.Lock()
		lastEventID := r.lastEventID
		r.mux.Unlock()
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}

		resp, err := r.client.Do(req)
		if err != nil {
			lastErr = err
			if !r.waitRetry(attempt, attempts) {
				return http.ErrServerClosed
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("unexpected status code: %d", resp.StatusCode)
			resp.Body.Close()
			if !r.waitRetry(attempt, attempts) {
				return http.ErrServerClosed
			}
			continue
		}

		if r.respHeader != nil {
			r.respHeader(resp.Header)
		}

		r.mux.Lock()
		if r.closed.Load() {
			r.mux.Unlock()
			resp.Body.Close()
			return http.ErrServerClosed
		}
		if r.body != nil {
			_ = r.body.Close()
		}
		r.body = resp.Body
		r.reader = bufio.NewReaderSize(resp.Body, 4096)
		r.mux.Unlock()
		return nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("failed to connect after %d attempts", attempts)
	}

	return fmt.Errorf("failed to connect after %d attempts: %w", attempts, lastErr)
}

func (r *HttpReceiver) waitRetry(attempt, attempts int) bool {
	if attempt >= attempts-1 || r.retryDelay <= 0 {
		return true
	}

	timer := time.NewTimer(r.retryDelay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-r.closeCh:
		return false
	}
}

func (r *HttpReceiver) closeBody() {
	r.mux.Lock()
	body := r.body
	r.body = nil
	r.reader = nil
	r.mux.Unlock()

	if body != nil {
		_ = body.Close()
	}
}

func (r *HttpReceiver) getReader() (*bufio.Reader, error) {
	r.mux.Lock()
	reader := r.reader
	r.mux.Unlock()

	if reader != nil {
		return reader, nil
	}

	if err := r.connect(); err != nil {
		return nil, err
	}

	r.mux.Lock()
	reader = r.reader
	r.mux.Unlock()
	if reader == nil {
		return nil, io.EOF
	}

	return reader, nil
}

type HttpReceiverOption func(*HttpReceiver)

func WithHttpReceiverClient(client *http.Client) HttpReceiverOption {
	return func(r *HttpReceiver) {
		r.client = client
	}
}

func WithHttpReceiverRetry(max int, delay time.Duration) HttpReceiverOption {
	return func(r *HttpReceiver) {
		r.retryMax = max
		r.retryDelay = delay
	}
}

func WithHttpReceiverRespHeader(respHeader func(header http.Header)) HttpReceiverOption {
	return func(r *HttpReceiver) {
		r.respHeader = respHeader
	}
}

func CreateHttpReceiver(receiverURL string, opts ...HttpReceiverOption) (*HttpReceiver, error) {
	parsedURL, err := url.Parse(receiverURL)
	if err != nil {
		return nil, err
	}

	receiver := &HttpReceiver{
		url:        receiverURL,
		client:     http.DefaultClient,
		requestURL: parsedURL,
		baseHeader: http.Header{
			"Accept":        []string{"text/event-stream"},
			"Cache-Control": []string{"no-cache"},
		},
		retryMax:   3,
		retryDelay: 1 * time.Second,
		closeCh:    make(chan struct{}),
	}

	for _, opt := range opts {
		opt(receiver)
	}

	if err := receiver.connect(); err != nil {
		return nil, err
	}

	return receiver, nil
}

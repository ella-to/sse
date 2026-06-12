package sse

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var LibVersion = "0.2.1"

const (
	// defaultReaderSize is the bufio.Reader buffer size used when parsing
	// streams. Lines shorter than this are parsed without any copying
	// beyond the final string conversion.
	defaultReaderSize = 4096

	// maxPushBufferRetain caps the scratch buffer capacity an HttpPusher
	// keeps alive between pushes, so a single large message does not pin
	// memory for the lifetime of the connection.
	maxPushBufferRetain = 64 << 10
)

type Message struct {
	Id    string
	Event string
	Data  string
}

// trimLineEnd strips a trailing LF or CRLF.
func trimLineEnd(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n = len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
}

// grow ensures dst has capacity for at least need bytes, at least doubling
// the capacity when reallocating. Plain append grows large slices by only
// ~1.25x, which costs noticeably more allocations when accumulating big
// payloads from small reads.
func grow(dst []byte, need int) []byte {
	if need <= cap(dst) {
		return dst
	}
	nb := make([]byte, len(dst), max(2*cap(dst), need, 64))
	copy(nb, dst)
	return nb
}

func appendGrow(dst, src []byte) []byte {
	dst = grow(dst, len(dst)+len(src))
	return append(dst, src...)
}

// readLine returns the next line from br without its line ending. The
// returned slice aliases br's internal buffer (or *spill for lines longer
// than that buffer) and is only valid until the next call.
func readLine(br *bufio.Reader, spill *[]byte) ([]byte, error) {
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

	buf := appendGrow((*spill)[:0], line)
	for {
		line, err = br.ReadSlice('\n')
		buf = appendGrow(buf, line)
		if err != bufio.ErrBufferFull {
			break
		}
	}
	*spill = buf

	if err != nil && err != io.EOF {
		return nil, err
	}

	return trimLineEnd(buf), err
}

// appendDataLine accumulates one data (or comment) line into msg. The first
// line is stored directly in msg.Data so single-line messages, by far the
// common case, never touch the multi-line accumulator.
func appendDataLine(msg *Message, dataBuf *[]byte, haveData *bool, value []byte) {
	if !*haveData {
		*haveData = true
		msg.Data = string(value)
		return
	}

	buf := *dataBuf
	if buf == nil {
		buf = make([]byte, 0, max(2*(len(msg.Data)+len(value)+1), 64))
		buf = append(buf, msg.Data...)
	}
	buf = grow(buf, len(buf)+len(value)+1)
	buf = append(buf, '\n')
	buf = append(buf, value...)
	*dataBuf = buf
}

func ReadMessage(r io.Reader) (*Message, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, defaultReaderSize)
	}

	msg := &Message{}
	var spill []byte   // long-line overflow, allocated only when needed
	var dataBuf []byte // multi-line data accumulator, allocated only when needed
	haveMessage := false
	haveData := false

	for {
		line, err := readLine(br, &spill)
		if err != nil && err != io.EOF {
			return nil, err
		}

		if len(line) == 0 {
			if haveMessage {
				if dataBuf != nil {
					msg.Data = string(dataBuf)
				}
				return msg, nil
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
			appendDataLine(msg, &dataBuf, &haveData, comment)
		} else {
			sep := bytes.IndexByte(line, ':')
			field := line
			var value []byte
			if sep >= 0 {
				field = line[:sep]
				value = line[sep+1:]
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
			}

			switch string(field) {
			case "data":
				haveMessage = true
				appendDataLine(msg, &dataBuf, &haveData, value)
			case "id":
				haveMessage = true
				if bytes.IndexByte(value, 0) < 0 {
					msg.Id = string(value)
				}
			case "event":
				haveMessage = true
				msg.Event = string(value)
			}
		}

		if err == io.EOF {
			if haveMessage {
				if dataBuf != nil {
					msg.Data = string(dataBuf)
				}
				return msg, nil
			}
			return nil, io.EOF
		}
	}
}

func WriteMessage(w io.Writer, msg *Message, buf *bytes.Buffer) error {
	// For large payloads, pre-size the buffer in one shot instead of paying
	// repeated grow-and-copy steps. For small payloads the extra scan of
	// msg.Data costs more than it saves, so skip it.
	if len(msg.Data) >= 1024 {
		size := 1 + len("id: \n") + len(msg.Id) + len("event: \n") + len(msg.Event) +
			len(msg.Data) + (strings.Count(msg.Data, "\n")+1)*len("data: \n")
		buf.Grow(size)
	}

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
		prefix := "data: "
		if isComment {
			prefix = ": "
		}
		data := msg.Data
		for {
			i := strings.IndexByte(data, '\n')
			buf.WriteString(prefix)
			if i < 0 {
				buf.WriteString(data)
				buf.WriteByte('\n')
				break
			}
			buf.WriteString(data[:i])
			buf.WriteByte('\n')
			data = data[i+1:]
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

var pingMessage = NewComment("ping")

type Pusher interface {
	Push(msg *Message) error
	Close() error
}

type HttpPusher struct {
	w            http.ResponseWriter
	flusher      http.Flusher
	closer       io.Closer
	pingDuration time.Duration
	pingTimer    *time.Timer
	closed       atomic.Bool
	mux          sync.Mutex
	buffer       bytes.Buffer
}

var _ Pusher = (*HttpPusher)(nil)

func (p *HttpPusher) Push(msg *Message) error {
	if p.closed.Load() {
		return http.ErrServerClosed
	}

	p.mux.Lock()
	defer p.mux.Unlock()

	if p.closed.Load() {
		return http.ErrServerClosed
	}

	p.buffer.Reset()
	err := WriteMessage(p.w, msg, &p.buffer)
	if p.buffer.Cap() > maxPushBufferRetain {
		p.buffer = bytes.Buffer{}
	}
	if err != nil {
		return err
	}

	p.flusher.Flush()

	// Reset the idle keepalive only after a successful write, so a dead
	// connection does not keep re-arming its own ping chain forever.
	if p.pingTimer != nil {
		p.pingTimer.Reset(p.pingDuration)
	}

	return nil
}

func (p *HttpPusher) Close() error {
	if p.closed.Swap(true) {
		return http.ErrServerClosed
	}

	p.mux.Lock()
	if p.pingTimer != nil {
		p.pingTimer.Stop()
		p.pingTimer = nil
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

	if pusher.pingDuration > 0 {
		pusher.pingTimer = time.AfterFunc(pusher.pingDuration, func() {
			_ = pusher.Push(pingMessage)
		})
	}

	return pusher, nil
}

type Receiver interface {
	Receive() (*Message, error)
	Close() error
}

type HttpReceiver struct {
	client     *http.Client
	req        *http.Request
	retryMax   int
	retryDelay time.Duration
	respHeader func(header http.Header)

	ctx    context.Context
	cancel context.CancelFunc

	lastEventID string
	closed      atomic.Bool
	mux         sync.Mutex
	recvMux     sync.Mutex
	body        io.ReadCloser
	reader      *bufio.Reader // reused across reconnects
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

	r.cancel()
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

		req := r.req.Clone(r.ctx)

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
		if r.reader == nil {
			r.reader = bufio.NewReaderSize(resp.Body, defaultReaderSize)
		} else {
			r.reader.Reset(resp.Body)
		}
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
	case <-r.ctx.Done():
		return false
	}
}

func (r *HttpReceiver) closeBody() {
	r.mux.Lock()
	body := r.body
	r.body = nil
	r.mux.Unlock()

	if body != nil {
		_ = body.Close()
	}
}

func (r *HttpReceiver) getReader() (*bufio.Reader, error) {
	r.mux.Lock()
	reader, connected := r.reader, r.body != nil
	r.mux.Unlock()

	if connected {
		return reader, nil
	}

	if err := r.connect(); err != nil {
		return nil, err
	}

	r.mux.Lock()
	reader, connected = r.reader, r.body != nil
	r.mux.Unlock()
	if !connected {
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
	ctx, cancel := context.WithCancel(context.Background())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, receiverURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	receiver := &HttpReceiver{
		client:     http.DefaultClient,
		req:        req,
		retryMax:   3,
		retryDelay: 1 * time.Second,
		ctx:        ctx,
		cancel:     cancel,
	}

	for _, opt := range opts {
		opt(receiver)
	}

	if err := receiver.connect(); err != nil {
		cancel()
		return nil, err
	}

	return receiver, nil
}

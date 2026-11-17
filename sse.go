package sse

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var LibVersion = "0.2.5"

const (
	// defaultReaderSize is the bufio.Reader buffer size used when parsing
	// streams. Lines shorter than this are parsed without any copying
	// beyond the final string conversion.
	defaultReaderSize = 4096

	// defaultReceiverBufferSize is the bufio.Reader size of an HttpReceiver.
	// Lines that fit are parsed without the spill copy; bus events routinely
	// exceed the 4 KB that ReadMessage uses for plain readers.
	defaultReceiverBufferSize = 32 << 10

	// defaultMaxMessageSize bounds a single message on an HttpReceiver.
	defaultMaxMessageSize = 16 << 20

	// maxReadBufferRetain caps the scratch capacity a receiver keeps between
	// messages.
	maxReadBufferRetain = 1 << 20

	// maxPushBufferRetain caps the scratch buffer capacity an HttpPusher
	// keeps alive between pushes, so a single large message does not pin
	// memory for the lifetime of the connection.
	maxPushBufferRetain = 64 << 10

	// maxStatusErrorBody caps how much of a non-200 response body is
	// captured into StatusError.Body.
	maxStatusErrorBody = 8 << 10
)

// ErrInvalidField is returned when a message id or event name contains a
// line break (or the id contains NUL). Writing such a value would split the
// frame and let the value inject extra fields or whole events.
var ErrInvalidField = errors.New("sse: id or event contains a line break or NUL")

// validField reports whether s can be written as a single-line field value.
func validField[T string | []byte](s T, isID bool) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n', '\r':
			return false
		case 0:
			if isID {
				return false
			}
		}
	}
	return true
}

// ErrMessageTooLarge is returned by HttpReceiver when a message exceeds the
// configured maximum size (see WithHttpReceiverMaxMessageSize). The
// connection is dropped; the next Receive reconnects.
var ErrMessageTooLarge = errors.New("sse: message too large")

// StatusError is returned (wrapped) by CreateHttpReceiver and Receive when
// the server answers a stream request with a status other than 200 OK.
// Use errors.As to inspect the code, for example to stop retrying on 401.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status code: %d", e.Code)
}

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
//
// A positive limit bounds how many bytes a spilled line may accumulate;
// longer lines fail with ErrMessageTooLarge.
func readLine(br *bufio.Reader, spill *[]byte, limit int) ([]byte, error) {
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
		if limit > 0 && len(buf) > limit {
			*spill = buf
			return nil, ErrMessageTooLarge
		}
	}
	*spill = buf

	if err != nil && err != io.EOF {
		return nil, err
	}

	return trimLineEnd(buf), err
}

// readState holds the scratch buffers of one stream reader so they are
// reused from message to message instead of being reallocated.
type readState struct {
	spill   []byte // long-line overflow
	dataBuf []byte // multi-line data accumulator
	multi   bool   // dataBuf holds the data of the current message
	maxSize int    // bytes allowed per message, 0 for no limit
	event   string // last event name seen, reused across messages
}

// release drops buffers that grew past maxReadBufferRetain, so one huge
// message does not pin memory for the lifetime of a stream.
func (st *readState) release() {
	if cap(st.spill) > maxReadBufferRetain {
		st.spill = nil
	}
	if cap(st.dataBuf) > maxReadBufferRetain {
		st.dataBuf = nil
	}
}

// appendData accumulates one data (or comment) line into msg. The first line
// is stored directly in msg.Data so single-line messages, by far the common
// case, never touch the multi-line accumulator.
func (st *readState) appendData(msg *Message, haveData *bool, value []byte) {
	if !*haveData {
		*haveData = true
		msg.Data = string(value)
		return
	}

	if !st.multi {
		st.multi = true
		st.dataBuf = appendGrow(st.dataBuf[:0], bytesOf(msg.Data))
	}
	st.dataBuf = grow(st.dataBuf, len(st.dataBuf)+len(value)+1)
	st.dataBuf = append(st.dataBuf, '\n')
	st.dataBuf = append(st.dataBuf, value...)
}

func ReadMessage(r io.Reader) (*Message, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, defaultReaderSize)
	}

	msg := &Message{}
	var st readState
	if err := readMessage(br, msg, &st); err != nil {
		return nil, err
	}
	return msg, nil
}

// readMessage parses the next message from br into msg, overwriting all of
// its fields, using st for scratch space.
func readMessage(br *bufio.Reader, msg *Message, st *readState) error {
	*msg = Message{}
	st.multi = false
	defer st.release()

	haveMessage := false
	haveData := false
	size := 0

	finish := func() {
		if st.multi {
			msg.Data = string(st.dataBuf)
		}
	}

	for {
		line, err := readLine(br, &st.spill, st.maxSize)
		if err != nil && err != io.EOF {
			return err
		}
		if size += len(line) + 1; st.maxSize > 0 && size > st.maxSize {
			return ErrMessageTooLarge
		}

		if len(line) == 0 {
			if haveMessage {
				finish()
				return nil
			}

			if err == io.EOF {
				return io.EOF
			}

			continue
		}

		if line[0] == ':' {
			haveMessage = true
			comment := line[1:]
			if len(comment) > 0 && comment[0] == ' ' {
				comment = comment[1:]
			}
			st.appendData(msg, &haveData, comment)
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
				st.appendData(msg, &haveData, value)
			case "id":
				haveMessage = true
				if bytes.IndexByte(value, 0) < 0 {
					msg.Id = string(value)
				}
			case "event":
				haveMessage = true
				// streams reuse a handful of event names; keep the last one
				// so repeated names do not allocate
				if string(value) != st.event {
					st.event = string(value)
				}
				msg.Event = st.event
			}
		}

		if err == io.EOF {
			if haveMessage {
				finish()
				return nil
			}
			return io.EOF
		}
	}
}

func WriteMessage(w io.Writer, msg *Message, buf *bytes.Buffer) error {
	frame, err := AppendMessage(buf.AvailableBuffer(), bytesOf(msg.Id), msg.Event, bytesOf(msg.Data))
	if err != nil {
		return err
	}
	buf.Write(frame)
	_, err = w.Write(buf.Bytes())
	return err
}

// AppendMessage appends one complete SSE frame (including the terminating
// blank line) to dst and returns the extended slice. A message with neither
// id nor event is written as a comment. It returns dst unchanged and
// ErrInvalidField if id or event cannot be written on a single line.
func AppendMessage(dst []byte, id []byte, event string, data []byte) ([]byte, error) {
	if !validField(id, true) || !validField(event, false) {
		return dst, ErrInvalidField
	}

	// For large payloads, pre-size the buffer in one shot instead of paying
	// repeated grow-and-copy steps. For small payloads the extra scan of
	// data costs more than it saves, so skip it.
	if len(data) >= 1024 {
		size := 1 + len("id: \n") + len(id) + len("event: \n") + len(event) +
			len(data) + (bytes.Count(data, newline)+1)*len("data: \n")
		dst = slices.Grow(dst, size)
	}

	isComment := true

	if len(id) > 0 {
		dst = append(dst, "id: "...)
		dst = append(dst, id...)
		dst = append(dst, '\n')
		isComment = false
	}

	if event != "" {
		dst = append(dst, "event: "...)
		dst = append(dst, event...)
		dst = append(dst, '\n')
		isComment = false
	}

	if len(data) == 0 {
		if !isComment {
			// without a data line browsers do not dispatch the event
			dst = append(dst, "data:\n"...)
		}
		return append(dst, '\n'), nil
	}

	prefix := "data: "
	if isComment {
		prefix = ": "
	}
	for {
		i := bytes.IndexByte(data, '\n')
		dst = append(dst, prefix...)
		if i < 0 {
			dst = append(dst, data...)
			dst = append(dst, '\n')
			break
		}
		dst = append(dst, data[:i]...)
		dst = append(dst, '\n')
		data = data[i+1:]
	}

	return append(dst, '\n'), nil
}

var newline = []byte{'\n'}

// bytesOf returns a read-only view of s without copying. The result must
// never be modified.
func bytesOf(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

func NewComment(data string) *Message {
	return &Message{
		Data: data,
	}
}

// pingFrame is the keepalive comment sent on idle connections.
var pingFrame = []byte(": ping\n\n")

type Pusher interface {
	Push(msg *Message) error
	Close() error
}

type HttpPusher struct {
	w            http.ResponseWriter
	rc           *http.ResponseController
	closer       io.Closer
	pingDuration time.Duration
	pingTimer    *time.Timer
	// active records that something was written since the last ping tick,
	// so the hot path never touches the timer. Guarded by mux.
	active       bool
	writeTimeout time.Duration
	closed       atomic.Bool
	// writing is set while a write or flush to the connection is in
	// progress, so Close can abort one that is stuck on a stalled peer.
	writing atomic.Bool
	mux     sync.Mutex
	buf     []byte // scratch frame buffer, reused across pushes
}

var _ Pusher = (*HttpPusher)(nil)

// Push writes msg and flushes it to the client.
func (p *HttpPusher) Push(msg *Message) error {
	return p.send(bytesOf(msg.Id), msg.Event, bytesOf(msg.Data), true)
}

// PushBytes is Push for a message held as byte slices. It writes and flushes
// one frame without converting data to a string; the slices are not retained.
func (p *HttpPusher) PushBytes(id []byte, event string, data []byte) error {
	return p.send(id, event, data, true)
}

// WriteMsg writes msg without flushing. Use it to send a burst of messages
// followed by a single Flush.
func (p *HttpPusher) WriteMsg(msg *Message) error {
	return p.send(bytesOf(msg.Id), msg.Event, bytesOf(msg.Data), false)
}

// WriteBytes is WriteMsg for a message held as byte slices.
func (p *HttpPusher) WriteBytes(id []byte, event string, data []byte) error {
	return p.send(id, event, data, false)
}

// PushEncoded writes a frame produced by AppendMessage and flushes it. The
// frame is written as is, so a fan-out server can encode an event once and
// send the same bytes to every connection. The slice is not retained.
func (p *HttpPusher) PushEncoded(frame []byte) error {
	return p.sendEncoded(frame, true)
}

// WriteEncoded is PushEncoded without the flush.
func (p *HttpPusher) WriteEncoded(frame []byte) error {
	return p.sendEncoded(frame, false)
}

func (p *HttpPusher) sendEncoded(frame []byte, flush bool) error {
	if p.closed.Load() {
		return http.ErrServerClosed
	}

	p.mux.Lock()
	defer p.mux.Unlock()

	if err := p.writeLocked(frame); err != nil {
		return err
	}
	if flush {
		if err := p.flushLocked(); err != nil {
			return err
		}
	}
	p.active = true
	return nil
}

// Flush sends everything written so far to the client.
func (p *HttpPusher) Flush() error {
	if p.closed.Load() {
		return http.ErrServerClosed
	}

	p.mux.Lock()
	defer p.mux.Unlock()

	return p.flushLocked()
}

func (p *HttpPusher) send(id []byte, event string, data []byte, flush bool) error {
	if p.closed.Load() {
		return http.ErrServerClosed
	}

	p.mux.Lock()
	defer p.mux.Unlock()

	frame, err := AppendMessage(p.buf[:0], id, event, data)
	if err != nil {
		return err
	}
	err = p.writeLocked(frame)
	if cap(frame) <= maxPushBufferRetain {
		p.buf = frame
	} else {
		p.buf = nil
	}
	if err != nil {
		return err
	}

	if flush {
		if err := p.flushLocked(); err != nil {
			return err
		}
	}

	p.active = true
	return nil
}

// beginIO marks a connection write as in progress and applies the write
// timeout. It fails if the pusher is closed. Must be called with p.mux held
// and paired with p.writing.Store(false).
//
// The store of writing before the load of closed pairs with Close storing
// closed before loading writing: at least one side observes the other, so
// Close never misses a write it would have to abort.
func (p *HttpPusher) beginIO() error {
	if p.writeTimeout > 0 {
		_ = p.rc.SetWriteDeadline(time.Now().Add(p.writeTimeout))
	}
	p.writing.Store(true)
	if p.closed.Load() {
		p.writing.Store(false)
		return http.ErrServerClosed
	}
	return nil
}

func (p *HttpPusher) writeLocked(frame []byte) error {
	if err := p.beginIO(); err != nil {
		return err
	}
	_, err := p.w.Write(frame)
	p.writing.Store(false)
	return err
}

func (p *HttpPusher) flushLocked() error {
	if err := p.beginIO(); err != nil {
		return err
	}
	err := p.rc.Flush()
	p.writing.Store(false)
	return err
}

// pingPeriod is half the ping duration: a tick sends a ping only if nothing
// was written since the previous tick, and a ping counts as traffic, so the
// connection is never idle for longer than pingDuration and an idle
// connection is pinged once per pingDuration.
func (p *HttpPusher) pingPeriod() time.Duration {
	return max(p.pingDuration/2, 1)
}

// pingTick runs on the ping timer. A failed ping write stops the chain, so a
// dead connection does not keep re-arming its own timer.
func (p *HttpPusher) pingTick() {
	p.mux.Lock()
	defer p.mux.Unlock()

	if p.pingTimer == nil || p.closed.Load() {
		return
	}

	if p.active {
		p.active = false
	} else {
		if err := p.writeLocked(pingFrame); err != nil {
			return
		}
		if err := p.flushLocked(); err != nil {
			return
		}
		p.active = true
	}

	p.pingTimer.Reset(p.pingPeriod())
}

// Close stops the pusher. A write blocked on a peer that stopped reading is
// aborted by moving the connection's write deadline into the past, so Close
// never waits on a stalled client.
func (p *HttpPusher) Close() error {
	if p.closed.Swap(true) {
		return http.ErrServerClosed
	}

	aborted := p.writing.Load()
	if aborted {
		_ = p.rc.SetWriteDeadline(time.Unix(1, 0))
	}

	p.mux.Lock()
	if p.pingTimer != nil {
		p.pingTimer.Stop()
		p.pingTimer = nil
	}
	if p.writeTimeout > 0 && !aborted {
		// let the server finish the response without our deadline
		_ = p.rc.SetWriteDeadline(time.Time{})
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

// WithHttpPusherWriteTimeout bounds every write and flush (pings included)
// with a connection write deadline, so a client that stops reading cannot
// block the pusher forever. Requires a ResponseWriter that supports
// http.ResponseController.SetWriteDeadline; otherwise it has no effect.
func WithHttpPusherWriteTimeout(d time.Duration) HttpPusherOption {
	return func(p *HttpPusher) {
		p.writeTimeout = d
	}
}

func CreateHttpPusher(w http.ResponseWriter, opts ...HttpPusherOption) (*HttpPusher, error) {
	pusher := &HttpPusher{
		w:  w,
		rc: http.NewResponseController(w),
	}
	if closer, ok := w.(io.Closer); ok {
		pusher.closer = closer
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// stop nginx and compatible proxies from buffering the stream
	w.Header().Set("X-Accel-Buffering", "no")

	for _, opt := range opts {
		opt(pusher)
	}

	// flushing sends the headers; it also detects writers that cannot stream
	if err := pusher.rc.Flush(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			return nil, http.ErrNotSupported
		}
		return nil, err
	}

	if pusher.pingDuration > 0 {
		pusher.mux.Lock()
		pusher.pingTimer = time.AfterFunc(pusher.pingPeriod(), pusher.pingTick)
		pusher.mux.Unlock()
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
	bufSize    int
	rs         readState // scratch buffers, guarded by recvMux
	respHeader func(header http.Header)
	reqHooks   []func(req *http.Request)

	parent context.Context // caller context, see WithHttpReceiverContext
	ctx    context.Context // parent plus Close
	cancel context.CancelFunc

	// recvMux serializes Receive. It guards everything on the read path,
	// so receiving a message takes no other lock.
	recvMux     sync.Mutex
	lastEventID string
	reader      *bufio.Reader // reused across reconnects
	connected   bool          // reader is attached to a live body

	closed atomic.Bool
	mux    sync.Mutex // guards body, which Close touches concurrently
	body   io.ReadCloser
}

var _ Receiver = (*HttpReceiver)(nil)

// Receive returns the next message, reconnecting (with Last-Event-ID) when
// the stream drops.
func (r *HttpReceiver) Receive() (*Message, error) {
	msg := &Message{}
	if err := r.ReceiveInto(msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// ReceiveInto is Receive without allocating a Message: every field of msg is
// overwritten with the next message. Reusing one msg across calls avoids an
// allocation per message; the strings it holds are never mutated.
func (r *HttpReceiver) ReceiveInto(msg *Message) error {
	r.recvMux.Lock()
	defer r.recvMux.Unlock()

	for {
		if err := r.done(); err != nil {
			return err
		}

		reader, err := r.getReader()
		if err != nil {
			if derr := r.done(); derr != nil {
				return derr
			}
			return err
		}

		err = readMessage(reader, msg, &r.rs)
		if err == nil {
			if msg.Id != "" {
				r.lastEventID = msg.Id
			}
			return nil
		}

		if derr := r.done(); derr != nil {
			return derr
		}

		r.connected = false
		r.closeBody()

		if errors.Is(err, ErrMessageTooLarge) {
			// the stream is mid-message; reconnecting would most likely
			// replay the same message, so let the caller decide
			return err
		}

		if err := r.connect(); err != nil {
			if derr := r.done(); derr != nil {
				return derr
			}
			return err
		}
	}
}

// done returns the error Receive should report once the receiver can no
// longer be used: http.ErrServerClosed after Close, or the caller context's
// error once it is cancelled. It returns nil while the receiver is usable.
func (r *HttpReceiver) done() error {
	if r.closed.Load() {
		return http.ErrServerClosed
	}
	return r.parent.Err()
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
		if err := r.done(); err != nil {
			return err
		}

		req := r.req.Clone(r.ctx)

		if r.lastEventID != "" {
			req.Header.Set("Last-Event-ID", r.lastEventID)
		}

		for _, hook := range r.reqHooks {
			hook(req)
		}

		resp, err := r.client.Do(req)
		if err != nil {
			lastErr = err
			if !r.waitRetry(attempt, attempts) {
				return r.done()
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxStatusErrorBody))
			resp.Body.Close()
			lastErr = &StatusError{Code: resp.StatusCode, Body: string(body)}
			if !r.waitRetry(attempt, attempts) {
				return r.done()
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
		r.mux.Unlock()

		if r.reader == nil {
			r.reader = bufio.NewReaderSize(resp.Body, r.bufSize)
		} else {
			r.reader.Reset(resp.Body)
		}
		r.connected = true
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

// getReader returns the reader of the current connection, connecting first
// if there is none. Must be called with recvMux held.
func (r *HttpReceiver) getReader() (*bufio.Reader, error) {
	if !r.connected {
		if err := r.connect(); err != nil {
			return nil, err
		}
	}
	return r.reader, nil
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

// WithHttpReceiverHeader sets a fixed header on every stream request,
// including reconnects.
func WithHttpReceiverHeader(key, value string) HttpReceiverOption {
	return func(r *HttpReceiver) {
		r.req.Header.Set(key, value)
	}
}

// WithHttpReceiverRequest registers a hook that runs on each stream request
// right before it is sent, after Last-Event-ID has been set. Use it for
// per-request values such as rotating bearer tokens. Hooks run in the order
// they were registered.
func WithHttpReceiverRequest(fn func(req *http.Request)) HttpReceiverOption {
	return func(r *HttpReceiver) {
		if fn != nil {
			r.reqHooks = append(r.reqHooks, fn)
		}
	}
}

// WithHttpReceiverBufferSize sets the read buffer size (default 32 KB).
// Lines longer than the buffer are still read, through an extra copy.
func WithHttpReceiverBufferSize(n int) HttpReceiverOption {
	return func(r *HttpReceiver) {
		if n > 0 {
			r.bufSize = n
		}
	}
}

// WithHttpReceiverMaxMessageSize bounds the size of a single message
// (default 16 MB); n <= 0 removes the limit. A larger message makes Receive
// return ErrMessageTooLarge and drops the connection.
func WithHttpReceiverMaxMessageSize(n int) HttpReceiverOption {
	return func(r *HttpReceiver) {
		r.rs.maxSize = max(n, 0)
	}
}

// WithHttpReceiverContext ties the receiver to ctx: connecting (including the
// initial connect in CreateHttpReceiver), reading and reconnecting stop when
// ctx is cancelled, and Receive then returns ctx.Err().
func WithHttpReceiverContext(ctx context.Context) HttpReceiverOption {
	return func(r *HttpReceiver) {
		if ctx != nil {
			r.parent = ctx
		}
	}
}

// WithHttpReceiverLastEventID seeds the Last-Event-ID sent on the initial
// connection, so a receiver can resume a stream from a cursor persisted by
// a previous receiver.
func WithHttpReceiverLastEventID(id string) HttpReceiverOption {
	return func(r *HttpReceiver) {
		r.lastEventID = id
	}
}

func CreateHttpReceiver(receiverURL string, opts ...HttpReceiverOption) (*HttpReceiver, error) {
	// the stored request is only a template; every attempt clones it with
	// the receiver's context
	req, err := http.NewRequest(http.MethodGet, receiverURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	receiver := &HttpReceiver{
		client:     http.DefaultClient,
		req:        req,
		retryMax:   3,
		retryDelay: 1 * time.Second,
		bufSize:    defaultReceiverBufferSize,
		rs:         readState{maxSize: defaultMaxMessageSize},
		parent:     context.Background(),
	}

	for _, opt := range opts {
		opt(receiver)
	}

	receiver.ctx, receiver.cancel = context.WithCancel(receiver.parent)

	if err := receiver.connect(); err != nil {
		receiver.cancel()
		if cerr := receiver.parent.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, err
	}

	return receiver, nil
}

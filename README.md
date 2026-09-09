```
░██████╗░██████╗███████╗
██╔════╝██╔════╝██╔════╝
╚█████╗░╚█████╗░█████╗░░
░╚═══██╗░╚═══██╗██╔══╝░░
██████╔╝██████╔╝███████╗
╚═════╝░╚═════╝░╚══════╝
```

<div align="center">

[![Go Reference](https://pkg.go.dev/badge/ella.to/sse.svg)](https://pkg.go.dev/ella.to/sse)
[![Go Report Card](https://goreportcard.com/badge/ella.to/sse)](https://goreportcard.com/report/ella.to/sse)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)


_A simple, optimized, and high-performance Server-Sent Events (SSE) client and server library for Go._

</div>

This package gives you:

- low-level message parsing/writing (`ReadMessage`, `WriteMessage`)
- HTTP server-side push (`HttpPusher`)
- HTTP client-side receive with reconnect (`HttpReceiver`)

Module path: `ella.to/sse`

## Install

```bash
go get ella.to/sse@v0.2.4
```

## API at a glance

### Message

```go
type Message struct {
    Id    string
    Event string
    Data  string
}
```

### Parser/Writer

```go
func ReadMessage(r io.Reader) (*Message, error)
func WriteMessage(w io.Writer, msg *Message, buf *bytes.Buffer) error
func NewComment(data string) *Message
```

### Pusher

```go
type Pusher interface {
    Push(msg *Message) error
    Close() error
}

func CreateHttpPusher(w http.ResponseWriter, opts ...HttpPusherOption) (*HttpPusher, error)
func WithHttpPusherHeader(key, value string) HttpPusherOption
func WithHttpPusherPingDuration(d time.Duration) HttpPusherOption
```

### Receiver

```go
type Receiver interface {
    Receive() (*Message, error)
    Close() error
}

func CreateHttpReceiver(url string, opts ...HttpReceiverOption) (*HttpReceiver, error)
func WithHttpReceiverClient(client *http.Client) HttpReceiverOption
func WithHttpReceiverRetry(max int, delay time.Duration) HttpReceiverOption
func WithHttpReceiverRespHeader(fn func(header http.Header)) HttpReceiverOption
func WithHttpReceiverHeader(key, value string) HttpReceiverOption
func WithHttpReceiverRequest(fn func(req *http.Request)) HttpReceiverOption
func WithHttpReceiverLastEventID(id string) HttpReceiverOption

// Returned (wrapped) when the server answers with a non-200 status.
type StatusError struct {
    Code int
    Body string
}
```

## Basic usage

### Server: send events over HTTP

```go
package main

import (
    "log"
    "net/http"
    "time"

    "ella.to/sse"
)

func streamHandler(w http.ResponseWriter, r *http.Request) {
    pusher, err := sse.CreateHttpPusher(
        w,
        sse.WithHttpPusherPingDuration(15*time.Second),
    )
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    defer pusher.Close()

    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    var n int
    for {
        select {
        case <-r.Context().Done():
            return
        case t := <-ticker.C:
            n++
            msg := &sse.Message{
                Id:    time.Now().UTC().Format(time.RFC3339Nano),
                Event: "tick",
                Data:  t.Format(time.RFC3339),
            }
            if err := pusher.Push(msg); err != nil {
                return
            }
        }
    }
}

func main() {
    http.HandleFunc("/events", streamHandler)
    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

### Client: receive events with reconnect

```go
package main

import (
    "errors"
    "fmt"
    "net/http"
    "time"

    "ella.to/sse"
)

func main() {
    const MaxRetry = 3
    const RetryDelay = 2 * time.Second

    receiver, err := sse.CreateHttpReceiver(
        "http://localhost:8080/events",
        sse.WithHttpReceiverRetry(MaxRetry, RetryDelay),
    )
    if err != nil {
        panic(err)
    }
    defer receiver.Close()

    for {
        msg, err := receiver.Receive()
        if errors.Is(err, http.ErrServerClosed) {
          return
        } else if err != nil {
            panic(err)
        }

        fmt.Printf("id=%s event=%s data=%q\n", msg.Id, msg.Event, msg.Data)
    }
}
```

### Client: auth headers, status errors, resuming from a cursor

```go
receiver, err := sse.CreateHttpReceiver(
    "http://localhost:8080/events",
    sse.WithHttpReceiverRetry(1, 0),
    // Fixed header on every request, including reconnects.
    sse.WithHttpReceiverHeader("X-Pipe-Peer", "peer-a"),
    // Per-request hook, runs right before each request is sent.
    sse.WithHttpReceiverRequest(func(req *http.Request) {
        req.Header.Set("Authorization", "Bearer "+currentToken())
    }),
    // Resume from an ID persisted by a previous receiver.
    sse.WithHttpReceiverLastEventID(savedCursor),
)

var statusErr *sse.StatusError
if errors.As(err, &statusErr) && statusErr.Code == http.StatusUnauthorized {
    // bad token: do not retry
}
```

## Behavior notes

- `Receive()` blocks until a message is available or an error happens.
- `HttpReceiver` reconnects when the stream breaks.
- `Last-Event-ID` is tracked from received message IDs and sent on reconnect. Seed the initial value with `WithHttpReceiverLastEventID`.
- Non-200 responses surface as `*sse.StatusError` (via `errors.As`) carrying the status code and up to 8 KiB of the response body.
- Calling `Close()` on receiver unblocks `Receive()` and closes the active connection.
- Calling `Close()` on pusher prevents further writes and closes the underlying writer when supported.

## Development

Run tests:

```bash
go test ./...
```

Run benchmarks:

```bash
go test -run ^$ -bench . -benchmem ./...
```

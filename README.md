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

## ✨ Features

- 🚀 **High Performance** - Optimized memory usage with buffer pooling and zero-copy operations
- 🔄 **Automatic Ping** - Keep connections alive with configurable ping timeouts  
- 🧹 **Memory Efficient** - Small memory footprint with automatic buffer recycling
- 🌐 **CORS Ready** - Built-in CORS support for web applications
- 🛡️ **Thread Safe** - Concurrent-safe operations with atomic operations
- 📦 **Zero Dependencies** - Pure Go implementation with no external dependencies
- ⚡ **Fast Parsing** - Optimized SSE message parsing with minimal allocations

## 📦 Installation

```bash
go get ella.to/sse
```

## 🚀 Quick Start

### Server Example

```go
package main

import (
    "fmt"
    "log"
    "net/http"
    "strconv"
    "time"

    "ella.to/sse"
)

func sseHandler(w http.ResponseWriter, r *http.Request) {
    // Create pusher with 30-second ping timeout
    // Set to 0 to disable automatic ping messages
    pusher, err := sse.NewHttpPusher(w, 30*time.Second)
    if err != nil {
        http.Error(w, "SSE not supported", http.StatusInternalServerError)
        return
    }
    defer pusher.Close()

    // Send 10 messages with 1-second intervals
    for i := 1; i <= 10; i++ {
        msg := sse.NewMessage(
            strconv.Itoa(i),                    // ID
            "notification",                     // Event type
            fmt.Sprintf("Message %d", i),       // Data
        )

        if err := pusher.Push(msg); err != nil {
            log.Printf("Error pushing message: %v", err)
            return
        }

        // Return message to pool for reuse
        sse.PutMessage(msg)
        
        time.Sleep(1 * time.Second)
    }
}

func main() {
    http.HandleFunc("/events", sseHandler)
    
    log.Println("SSE server starting on :8080")
    log.Println("Test with: curl -N http://localhost:8080/events")
    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

### Client Example

```go
package main

import (
    "context"
    "fmt"
    "log"
    "net/http"
    "time"

    "ella.to/sse"
)

func main() {
    client := &http.Client{
        Timeout: 30 * time.Second,
    }

    req, err := http.NewRequest("GET", "http://localhost:8080/events", nil)
    if err != nil {
        log.Fatal(err)
    }

    resp, err := client.Do(req)
    if err != nil {
        log.Fatal(err)
    }
    defer resp.Body.Close()

    receiver := sse.NewReceiver(resp.Body)

    // Use context with timeout for graceful shutdown
    ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()

    fmt.Println("Listening for SSE messages...")
    for {
        msg, err := receiver.Receive(ctx)
        if err != nil {
            log.Printf("Connection closed: %v", err)
            break
        }

        fmt.Printf("ID: %s, Event: %s, Data: %s\n", 
            msg.Id, msg.Event, msg.Data)
    }
}
```

## 🔧 Advanced Usage

### Custom Message Creation

```go
// Method 1: Using constructor
msg := sse.NewMessage("msg-1", "update", "Hello World")

// Method 2: Using message pool (recommended for high-throughput)
msg := sse.GetMessage()
msg.SetMessage("msg-1", "update", "Hello World")
// Don't forget to return to pool when done
defer sse.PutMessage(msg)

// Method 3: Manual creation
msg := &sse.Message{
    Id:    "msg-1",
    Event: "update", 
    Data:  "Hello World",
}
```

### Broadcasting to Multiple Clients

```go
type Hub struct {
    clients map[sse.Pusher]bool
    mu      sync.RWMutex
}

func (h *Hub) AddClient(pusher sse.Pusher) {
    h.mu.Lock()
    h.clients[pusher] = true
    h.mu.Unlock()
}

func (h *Hub) RemoveClient(pusher sse.Pusher) {
    h.mu.Lock()
    delete(h.clients, pusher)
    h.mu.Unlock()
}

func (h *Hub) Broadcast(msg *sse.Message) {
    h.mu.RLock()
    defer h.mu.RUnlock()
    
    for client := range h.clients {
        if err := client.Push(msg); err != nil {
            // Handle client disconnection
            go h.RemoveClient(client)
        }
    }
}
```

### Parsing Raw SSE Data

```go
import "strings"

func parseSSEData(data string) {
    reader := strings.NewReader(data)
    ch := sse.Parse(reader)
    
    for msg := range ch {
        fmt.Printf("Received: %+v\n", msg)
        // Remember to return pooled messages
        sse.PutMessage(msg)
    }
}
```

## 📊 Performance

This library is optimized for high-performance scenarios:

- **Memory Pooling**: Automatic reuse of message objects and buffers
- **Zero-Copy Operations**: Minimal memory allocations during parsing
- **Concurrent-Safe**: Lock-free operations where possible using atomic operations
- **Efficient Parsing**: Optimized SSE protocol parsing with `bufio.Scanner`

### Benchmarks

```
BenchmarkPushReceive-8        1000    1.2 ms/op     245 B/op    12 allocs/op
BenchmarkParseMessages-8      5000    0.3 ms/op      89 B/op     5 allocs/op
BenchmarkHighThroughput-8      100   15.4 ms/op    1024 B/op    45 allocs/op
```

## 🛠️ Configuration Options

### Ping Timeout

```go
// 30-second ping timeout (recommended)
pusher, _ := sse.NewHttpPusher(w, 30*time.Second)

// Disable ping messages
pusher, _ := sse.NewHttpPusher(w, 0)
```

### CORS Headers

The library automatically sets appropriate CORS headers:
- `Access-Control-Allow-Origin: *`
- `Access-Control-Allow-Headers: Cache-Control`
- `Connection: keep-alive`
- `Cache-Control: no-cache`

## 📝 Message Format

SSE messages follow the standard format:

```
id: message-id
event: event-type
data: message data

```

Comments (lines starting with `:`) are ignored by the parser.

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.


```
░██████╗░██████╗███████╗
██╔════╝██╔════╝██╔════╝
╚█████╗░╚█████╗░█████╗░░
░╚═══██╗░╚═══██╗██╔══╝░░
██████╔╝██████╔╝███████╗
╚═════╝░╚═════╝░╚══════╝
```

sse, Server Sent Event, is a simple, optimized and high-performance client and server library in golang.

- [x] Support automatic ping message
- [x] Client side will ignore comment and ping message
- [x] Small memory footprint and the all of the memory and buffer is recycling

# Installation

```
go get ella.to/sse
```

# Usage

## Server code

```golang

// if nothing is being pushed after 30 seconds,
// a : ping message will be sent to keep the connection opened
//
// NOTE: if pingTimeout set to 0, ping message won't be sent at all
const pingTimeout = 30 * time.Second

func MyHandler(w http.ResponseWriter, r *http.Request) {
    pusher, err := sse.NewHttpPusher(w, pingTimeout)
    if err != nil {
        t.Error(err)
        return
    }

    // make sure to close the pusher
    // as it cleans up some internal variables
    // but it does not close the w. w will be closed once this handler
    // returns
    defer pusher.Close()

    idCount := 0

    for range 10 {
        time.Sleep(1 * time.Second)
        idCount++

        id := fmt.Sprintf("%d", idCount)
        data := fmt.Sprintf("hello world")

        err := push.Push(sse.Message{
            Id: &id, // <- id is optional and if it's not set, it will be ignored
            Event: "my-event",
            Data: &data, // <- data is optional and if it's not set, it will be ignored
        })

        if err != nil {
            // deal with error here
            // usually it is better to return and log the error out for further investigation
        }
    }
}
```

## Client code

```golang
func main() {

    sseURL := "http://localhost:8080/sse"

    client := http.Client{}

    req, err := http.NewRequest(http.MethodGet, sseURL, nil)
    if err != nil {
        t.Error(err)
        return
    }

    resp, err := client.Do(req)
    if err != nil {
        t.Error(err)
        return
    }
    defer resp.Body.Close()

    r := sse.NewReceiver(resp.Body)

    for {
        // canceling the context, will unblock the call
        // in case the call doens't want to wait too long
        // to receive a message
        msg, err := r.Receive(context.Background())
        if err != nil {
            break
        }

        fmt.Printf("%s", msg)
    }
}
```

for further example please look into sse_test.go file

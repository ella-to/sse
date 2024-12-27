package sse_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ella.to/sse"
)

func TestParse(t *testing.T) {
	r := strings.NewReader("data: hello\n\n")
	ch := sse.Parse(r)

	msg := <-ch
	if msg.Id != nil {
		t.Errorf("Expected Id to be nil, got %s", *msg.Id)
	}
	if msg.Event != "" {
		t.Errorf("Expected Event to be empty, got %s", msg.Event)
	}
	if *msg.Data != "hello" {
		t.Errorf("Expected Data to be hello, got %s", *msg.Data)
	}
}

func TestParseLarge(t *testing.T) {
	file, err := os.Open("./testdata/test01.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	ch := sse.Parse(file)

	for msg := range ch {
		fmt.Printf("%s", msg)
	}
}

func TestPusherReceiver(t *testing.T) {
	n := 100000
	c := 10

	var wg sync.WaitGroup

	wg.Add(c)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pusher, err := sse.NewPusher(w, 10*time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer pusher.Close()

		var msg sse.Message

		for i := range n {
			id := fmt.Sprintf("id-%d", i)
			event := "event"
			data := fmt.Sprintf("data-%d", i)

			msg.Id = &id
			msg.Event = event
			msg.Data = &data

			err = pusher.Push(&msg)
			if err != nil {
				break
			}
		}
	}))
	defer server.Close()

	client := http.Client{}

	for range c {
		go func() {
			defer wg.Done()

			req, err := http.NewRequest(http.MethodGet, server.URL, nil)
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
				msg, err := r.Receive(context.Background())
				if err != nil {
					break
				}

				_ = msg
				// fmt.Printf("%s", msg)
			}
		}()
	}

	wg.Wait()
}

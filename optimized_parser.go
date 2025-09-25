package sse

import (
	"bufio"
	"io"
)

// FastParse provides an even more optimized parsing implementation
// that reduces allocations further by reusing more components
func FastParse(r io.Reader) <-chan *Message {
	ch := make(chan *Message, 32) // Larger buffer for higher throughput

	// Use a single reusable buffer for the entire parsing session
	buffer := make([]byte, 8192) // 8KB buffer

	go func() {
		defer close(ch)

		scanner := bufio.NewScanner(r)
		scanner.Buffer(buffer, 65536) // 64KB max token

		// Reuse byte slices for field parsing
		var fieldBuf, valueBuf []byte

		for {
			msg := GetMessage()
			hasContent := false

			for scanner.Scan() {
				line := scanner.Bytes()

				// Empty line indicates end of message
				if len(line) == 0 {
					break
				}

				// Comment line (starts with :)
				if len(line) > 0 && line[0] == ':' {
					continue
				}

				// Find colon and space efficiently
				colonIdx := -1
				for i := 0; i < len(line)-1; i++ {
					if line[i] == ':' && line[i+1] == ' ' {
						colonIdx = i
						break
					}
				}

				if colonIdx == -1 {
					continue
				}

				fieldBuf = line[:colonIdx]
				valueBuf = line[colonIdx+2:]

				// Fast field matching using length and first character
				switch {
				case len(fieldBuf) == 2 && fieldBuf[0] == 'i': // "id"
					msg.Id = string(valueBuf)
					hasContent = true
				case len(fieldBuf) == 5 && fieldBuf[0] == 'e': // "event"
					msg.Event = string(valueBuf)
					hasContent = true
				case len(fieldBuf) == 4 && fieldBuf[0] == 'd': // "data"
					msg.Data = string(valueBuf)
					hasContent = true
				}
			}

			if err := scanner.Err(); err != nil {
				PutMessage(msg)
				if err != io.EOF {
					// Could log error here
				}
				return
			}

			if !hasContent {
				PutMessage(msg)
				return // EOF or no more messages
			}

			ch <- msg

			if msg.Event == "done" {
				return
			}
		}
	}()

	return ch
}

// BatchParse processes multiple messages in batches to reduce channel overhead
func BatchParse(r io.Reader, batchSize int) <-chan []*Message {
	ch := make(chan []*Message, 4)

	go func() {
		defer close(ch)

		msgCh := FastParse(r)
		batch := make([]*Message, 0, batchSize)

		for msg := range msgCh {
			batch = append(batch, msg)

			if len(batch) >= batchSize {
				ch <- batch
				batch = make([]*Message, 0, batchSize)
			}
		}

		// Send remaining messages
		if len(batch) > 0 {
			ch <- batch
		}
	}()

	return ch
}

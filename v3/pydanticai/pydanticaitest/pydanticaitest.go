// Package pydanticaitest decodes the server-sent events of the AGUI and Vercel
// handlers in tests.
package pydanticaitest

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/Kludex/pydantic-ai-go/ai/ui/vercel"
)

// Record is one record of a server-sent events stream.
type Record struct {
	// Data is the data lines of the record, joined by a newline.
	Data string

	// Heartbeat is true for a record without data lines, such as a comment line.
	Heartbeat bool
}

// Reader reads the records of a server-sent events stream one at a time.
type Reader struct{ scanner *bufio.Scanner }

func NewReader(stream io.Reader) *Reader {
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64<<10), math.MaxInt)
	return &Reader{scanner: scanner}
}

// Next returns the next record, or false at the end or on an error (see Err).
func (r *Reader) Next() (Record, bool) {
	var (
		data  []string
		lines int
	)
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if line == "" {
			if lines == 0 {
				continue
			}
			return Record{Data: strings.Join(data, "\n"), Heartbeat: len(data) == 0}, true
		}
		lines++
		// Field parsing follows the server-sent events format.
		if name, value, _ := strings.Cut(line, ":"); name == "data" {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	return Record{}, false
}

// NextData is Next for the data of the records that have any, skipping heartbeats.
func (r *Reader) NextData() (string, bool) {
	for {
		record, ok := r.Next()
		if !ok {
			return "", false
		}
		if !record.Heartbeat {
			return record.Data, true
		}
	}
}

// Err returns the error that ended the stream, if any.
func (r *Reader) Err() error { return r.scanner.Err() }

// AGUIEvents decodes the events in the body of an AG-UI response, and fails the
// test on an error.
func AGUIEvents(t testing.TB, body io.Reader) []agui.Event {
	t.Helper()

	reader := NewReader(body)
	var events []agui.Event
	for data, ok := reader.NextData(); ok; data, ok = reader.NextData() {
		var event agui.Event
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatalf("pydanticaitest: decode AG-UI event %q: %v", data, err)
		}
		events = append(events, event)
	}
	if err := reader.Err(); err != nil {
		t.Fatalf("pydanticaitest: read the stream: %v", err)
	}
	return events
}

// AGUIText joins the text of the messages in events.
func AGUIText(events []agui.Event) string {
	var text strings.Builder
	for _, event := range events {
		if event.Type == agui.EventTextMessageContent {
			delta, _ := event.Delta.(string)
			text.WriteString(delta)
		}
	}
	return text.String()
}

// VercelChunks decodes the chunks in the body of a Vercel AI response; done
// reports whether [DONE] ended it. It fails the test on an error.
func VercelChunks(t testing.TB, body io.Reader) (chunks []vercel.Chunk, done bool) {
	t.Helper()

	reader := NewReader(body)
	for data, ok := reader.NextData(); ok; data, ok = reader.NextData() {
		if data == "[DONE]" {
			done = true
			continue
		}
		if done {
			t.Fatalf("pydanticaitest: chunk after [DONE]: %s", data)
		}
		var chunk vercel.Chunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("pydanticaitest: decode Vercel AI chunk %q: %v", data, err)
		}
		chunks = append(chunks, chunk)
	}
	if err := reader.Err(); err != nil {
		t.Fatalf("pydanticaitest: read the stream: %v", err)
	}
	return chunks, done
}

// VercelText joins the text of the chunks.
func VercelText(chunks []vercel.Chunk) string {
	var text strings.Builder
	for _, chunk := range chunks {
		if chunk.Type == vercel.ChunkTextDelta {
			text.WriteString(chunk.Delta)
		}
	}
	return text.String()
}

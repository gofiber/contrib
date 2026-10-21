// Package pydanticaitest helps to test the handlers of the pydanticai package.
//
// AGUI and Vercel answer with server-sent events. The functions of this
// package decode them, so that a test can assert on the events of a run and on
// its text instead of on raw bytes:
//
//	resp, err := app.Test(req)
//	require.NoError(t, err)
//	chunks, done := pydanticaitest.VercelChunks(t, resp.Body)
//	require.True(t, done)
//	require.Equal(t, "Hello!", pydanticaitest.VercelText(chunks))
package pydanticaitest

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/Kludex/pydantic-ai-go/ai/ui/vercel"
)

// Record is one record of a server-sent events stream.
type Record struct {
	// Data is the data lines of the record, joined by a newline.
	Data string

	// Heartbeat is true for a record without data lines, such as the comment
	// lines that the handlers write while the agent is busy.
	Heartbeat bool
}

// Reader reads the records of a server-sent events stream one at a time. A
// test can use it to look at a stream while the handler is still writing it.
type Reader struct{ scanner *bufio.Scanner }

// NewReader returns a Reader for stream.
func NewReader(stream io.Reader) *Reader {
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return &Reader{scanner: scanner}
}

// Next returns the next record. It returns false when the stream has ended, or
// could not be read, see Err. A record that the stream was cut off in is
// dropped.
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
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			data = append(data, value)
		}
	}
	return Record{}, false
}

// NextData returns the data of the next record that has any, and skips the
// heartbeats. It returns false when the stream has ended, or could not be read,
// see Err.
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

// Err returns the error that ended the stream, or nil when it ended at its end.
func (r *Reader) Err() error { return r.scanner.Err() }

// AGUIEvents reads the body of an AG-UI response to its end and decodes the
// events in it. It fails the test when the body cannot be read or an event
// cannot be decoded.
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

// VercelChunks reads the body of a Vercel AI response to its end and decodes
// the chunks in it. done reports whether the stream ended with the [DONE]
// record. It fails the test when the body cannot be read, a chunk cannot be
// decoded, or a chunk follows [DONE].
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

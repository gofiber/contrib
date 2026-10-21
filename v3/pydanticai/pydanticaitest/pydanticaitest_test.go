package pydanticaitest_test

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/Kludex/pydantic-ai-go/ai/ui/agui"
	"github.com/Kludex/pydantic-ai-go/ai/ui/vercel"
	"github.com/gofiber/contrib/v3/pydanticai"
	"github.com/gofiber/contrib/v3/pydanticai/pydanticaitest"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

// recorder is a testing.TB that records how a test failed, instead of failing.
type recorder struct {
	testing.TB
	failure string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failure = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// failure runs fn as a test and returns the message that it failed with.
func failure(fn func(t testing.TB)) string {
	r := new(recorder)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r)
	}()
	<-done
	return r.failure
}

func Test_Reader(t *testing.T) {
	t.Parallel()

	stream := ": heartbeat\n\n" +
		"\n\n" + // empty lines between records are not records
		"data: a\n\n" +
		"event: custom\ndata: b1\ndata: b2\n\n" +
		"retry: 100\n\n" +
		"data: cut off"
	reader := pydanticaitest.NewReader(strings.NewReader(stream))

	record, ok := reader.Next()
	require.True(t, ok)
	require.Equal(t, pydanticaitest.Record{Heartbeat: true}, record)

	record, ok = reader.Next()
	require.True(t, ok)
	require.Equal(t, pydanticaitest.Record{Data: "a"}, record)

	record, ok = reader.Next()
	require.True(t, ok)
	require.Equal(t, pydanticaitest.Record{Data: "b1\nb2"}, record, "the data lines of a record are joined")

	data, ok := reader.NextData()
	require.False(t, ok, "heartbeats are skipped, and the record that was cut off is dropped")
	require.Empty(t, data)
	require.NoError(t, reader.Err())
}

func Test_Reader_LongRecords(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 200<<10) // above the default limit of a line of bufio.Scanner
	reader := pydanticaitest.NewReader(strings.NewReader("data: " + long + "\n\n"))

	data, ok := reader.NextData()
	require.True(t, ok)
	require.Equal(t, long, data)
}

func Test_Reader_Err(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset")
	reader := pydanticaitest.NewReader(iotest.ErrReader(boom))

	_, ok := reader.Next()
	require.False(t, ok)
	require.ErrorIs(t, reader.Err(), boom)
}

func Test_AGUI(t *testing.T) {
	t.Parallel()

	body := `: heartbeat

data: {"type":"RUN_STARTED","threadId":"t","runId":"r"}

data: {"type":"TEXT_MESSAGE_CONTENT","messageId":"m","delta":"Hel"}

data: {"type":"TEXT_MESSAGE_CONTENT","messageId":"m","delta":"lo"}

data: {"type":"RUN_FINISHED"}

`
	events := pydanticaitest.AGUIEvents(t, strings.NewReader(body))
	require.Len(t, events, 4, "the heartbeat is not an event")
	require.Equal(t, agui.EventRunStarted, events[0].Type)
	require.Equal(t, agui.EventRunFinished, events[3].Type)
	require.Equal(t, "Hello", pydanticaitest.AGUIText(events))

	require.Empty(t, pydanticaitest.AGUIEvents(t, strings.NewReader("")))
	require.Empty(t, pydanticaitest.AGUIText(nil))
}

func Test_AGUI_Fails(t *testing.T) {
	t.Parallel()

	message := failure(func(t testing.TB) {
		pydanticaitest.AGUIEvents(t, strings.NewReader("data: {not json\n\n"))
	})
	require.Contains(t, message, "decode AG-UI event")
	require.Contains(t, message, "{not json")

	message = failure(func(t testing.TB) {
		pydanticaitest.AGUIEvents(t, iotest.ErrReader(errors.New("connection reset")))
	})
	require.Contains(t, message, "read the stream: connection reset")
}

func Test_Vercel(t *testing.T) {
	t.Parallel()

	body := `data: {"type":"start"}

data: {"type":"text-delta","id":"t","delta":"Hel"}

: heartbeat

data: {"type":"text-delta","id":"t","delta":"lo"}

data: {"type":"finish"}

data: [DONE]

`
	chunks, done := pydanticaitest.VercelChunks(t, strings.NewReader(body))
	require.True(t, done)
	require.Len(t, chunks, 4)
	require.Equal(t, vercel.ChunkStart, chunks[0].Type)
	require.Equal(t, "Hello", pydanticaitest.VercelText(chunks))

	chunks, done = pydanticaitest.VercelChunks(t, strings.NewReader(`data: {"type":"abort"}`+"\n\n"))
	require.False(t, done, "a stream without [DONE] is reported")
	require.Len(t, chunks, 1)

	require.Empty(t, pydanticaitest.VercelText(nil))
}

func Test_Vercel_Fails(t *testing.T) {
	t.Parallel()

	message := failure(func(t testing.TB) {
		pydanticaitest.VercelChunks(t, strings.NewReader("data: {not json\n\n"))
	})
	require.Contains(t, message, "decode Vercel AI chunk")

	message = failure(func(t testing.TB) {
		pydanticaitest.VercelChunks(t, strings.NewReader("data: [DONE]\n\ndata: {\"type\":\"start\"}\n\n"))
	})
	require.Contains(t, message, "chunk after [DONE]")

	message = failure(func(t testing.TB) {
		pydanticaitest.VercelChunks(t, iotest.ErrReader(errors.New("connection reset")))
	})
	require.Contains(t, message, "read the stream: connection reset")
}

// The functions decode what the handlers write.
func Test_HandlerResponses(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](fakes.NewTestModel())
	app := fiber.New()
	app.Post("/agui", pydanticai.AGUI(agent))
	app.Post("/chat", pydanticai.Vercel(agent))

	// post returns the body of the response of app.
	post := func(path, body string) io.Reader {
		t.Helper()
		request := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader(body))
		request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		resp, err := app.Test(request)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp.Body
	}

	events := pydanticaitest.AGUIEvents(t, post("/agui",
		`{"threadId":"t","runId":"r","messages":[{"id":"u","role":"user","content":"Hello"}]}`))
	require.Equal(t, agui.EventRunStarted, events[0].Type)
	require.Equal(t, agui.EventRunFinished, events[len(events)-1].Type)
	require.Equal(t, "success (no tool calls)", pydanticaitest.AGUIText(events))

	chunks, done := pydanticaitest.VercelChunks(t, post("/chat",
		`{"trigger":"submit-message","id":"chat-1","messages":[{"id":"u","role":"user","parts":[{"type":"text","text":"Hello"}]}]}`))
	require.True(t, done)
	require.Equal(t, "success (no tool calls)", pydanticaitest.VercelText(chunks))
}

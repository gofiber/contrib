package agentrun_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/fiber/v3/log"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a log destination that the goroutines of a run can write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends the log of Fiber to the returned buffer until the test ends.
// The logger is global, so the test must not run in parallel with others.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()

	logs := new(syncBuffer)
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return logs
}

func Test_PanicError(t *testing.T) {
	t.Parallel()

	err := agentrun.PanicError("kaboom")
	require.EqualError(t, err, "pydanticai: panic in the run: kaboom")

	// A panic with an error does not make the error the cause: it would turn
	// the panic into a retry or a failure of a tool.
	cause := errors.New("cause")
	require.NotErrorIs(t, agentrun.PanicError(cause), cause)
}

func Test_Safely(t *testing.T) {
	logs := captureLog(t)

	called := false
	agentrun.Safely("Callback", func() { called = true })
	require.True(t, called)
	require.Empty(t, logs.String())

	require.NotPanics(t, func() { agentrun.Safely("Callback", func() { panic("bug in Callback") }) })
	require.Contains(t, logs.String(), "panic in Callback: bug in Callback")
	require.Contains(t, logs.String(), "goroutine ", "the stack of the panic")
}

func Test_Text(t *testing.T) {
	logs := captureLog(t)

	failure := errors.New("provider rejected the key sk-secret")
	require.Equal(t, failure.Error(), agentrun.Text(nil, failure))
	require.Equal(t, "the run failed", agentrun.Text(func(error) string { return "the run failed" }, failure))

	// The client is told what it is told for a panic, not what message hides.
	text := agentrun.Text(func(error) string { panic("bug in ErrorMessage") }, failure)
	require.Equal(t, agentrun.ErrPanicked.Error(), text)
	require.Contains(t, logs.String(), "panic in ErrorMessage: bug in ErrorMessage")
}

func answering(text string) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(context.Context, []ai.ModelMessage, ai.ModelRequestParams) (*ai.ModelResponse, error) {
		return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: text}}}, nil
	})
}

func Test_Track(t *testing.T) {
	t.Parallel()

	t.Run("not wanted", func(t *testing.T) {
		t.Parallel()

		options := []ai.RunOption{ai.WithRunMetadata(map[string]any{"k": "v"})}
		tracked, tracker := agentrun.Track(options, false)
		require.Nil(t, tracker)
		require.Equal(t, len(options), len(tracked))
	})

	t.Run("wanted", func(t *testing.T) {
		t.Parallel()

		// A slice with room to spare, which an append would write into.
		options := make([]ai.RunOption, 1, 8)
		options[0] = ai.WithRunMetadata(map[string]any{"k": "v"})
		tracked, tracker := agentrun.Track(options, true)
		require.Len(t, tracked, 2)
		require.Nil(t, options[:2][1], "the slice of the caller is not written to")
		require.Nil(t, tracker.Run(), "no run has started")

		agent := ai.NewAgent[struct{}, string](answering("done"))
		result, err := agent.Run(context.Background(), "Hello", struct{}{}, tracked...)
		require.NoError(t, err)
		run := tracker.Run()
		require.NotNil(t, run)
		require.Equal(t, result.RunID(), run.RunID)
	})
}

// toolCalls is a model that asks for one call of each named tool in its first
// response, so that the calls run concurrently, and answers "done" afterwards.
func toolCalls(names ...string) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(
		_ context.Context, messages []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		for _, message := range messages {
			if _, ok := message.(ai.ModelResponse); ok {
				return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: "done"}}}, nil
			}
		}
		parts := make([]ai.ResponsePart, len(names))
		for i, name := range names {
			parts[i] = ai.ToolCallPart{ToolName: name, Args: json.RawMessage(`{}`), ToolCallID: name}
		}
		return &ai.ModelResponse{Parts: parts}, nil
	})
}

func Test_GuardTools(t *testing.T) {
	logs := captureLog(t)

	agent := ai.NewAgent[struct{}, string](toolCalls("explode", "waiting"))
	ai.AddSimpleTool(agent, "explode", func(context.Context, struct{}) (string, error) { panic("kaboom") })
	ai.AddSimpleTool(agent, "waiting", func(ctx context.Context, _ struct{}) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})

	options := []ai.RunOption{ai.WithRunMetadata(map[string]any{"k": "v"})}
	guarded, guard := agentrun.GuardTools(options)
	require.Len(t, guarded, 2)
	require.Len(t, options, 1, "the slice of the caller is not written to")
	require.NoError(t, guard.Err(), "nothing has panicked")

	// The two tools run on goroutines of their own: without the guard the
	// panic would end the process.
	_, err := agent.Run(context.Background(), "Hello", struct{}{}, guarded...)
	require.Error(t, err)
	require.ErrorContains(t, guard.Err(), "panic in the run: kaboom")
	require.Contains(t, logs.String(), "panic in the run: kaboom")
	require.Contains(t, logs.String(), "goroutine ", "the stack of the panic")
}

func Test_GuardTools_NoPanic(t *testing.T) {
	t.Parallel()

	agent := ai.NewAgent[struct{}, string](toolCalls("fine"))
	ai.AddSimpleTool(agent, "fine", func(context.Context, struct{}) (string, error) { return "ok", nil })

	guarded, guard := agentrun.GuardTools(nil)
	result, err := agent.Run(context.Background(), "Hello", struct{}{}, guarded...)
	require.NoError(t, err)
	require.Equal(t, "done", strings.TrimSpace(result.Output))
	require.NoError(t, guard.Err())
}

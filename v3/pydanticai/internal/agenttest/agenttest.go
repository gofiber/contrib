// Package agenttest holds what the tests of the packages of this module share:
// the models and agents that they run, and the capture of the log.
package agenttest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/models/fakes"
	"github.com/gofiber/fiber/v3/log"
)

// ObserveTimeout bounds how long a test waits for something to happen in the
// background.
const ObserveTimeout = 5 * time.Second

// Receive waits for a value from channel or fails the test.
func Receive[T any](t testing.TB, channel <-chan T, what string) T {
	t.Helper()
	return receive(t, channel, what, ObserveTimeout)
}

func receive[T any](t testing.TB, channel <-chan T, what string, timeout time.Duration) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(timeout):
	}
	t.Fatalf("timed out waiting for %s", what)
	var zero T
	return zero
}

// SyncBuffer is a log destination that can be read while it is written.
type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what was written.
func (b *SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// CaptureLog sends the log of Fiber to the returned buffer until the test ends.
// The logger is global, so the test must not run in parallel with others.
func CaptureLog(t testing.TB) *SyncBuffer {
	t.Helper()

	logs := new(SyncBuffer)
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return logs
}

// Answering is a model that always gives the same answer.
func Answering(text string) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(context.Context, []ai.ModelMessage, ai.ModelRequestParams) (*ai.ModelResponse, error) {
		return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: text}}}, nil
	})
}

// Deadline is the time left until the deadline of the context of a model call,
// or Set false when the context had none.
type Deadline struct {
	Left time.Duration
	Set  bool
}

// Require fails the test unless the deadline is the given time away, up to the
// time that the model call took to start. Zero means no deadline at all.
func (d Deadline) Require(t testing.TB, want time.Duration) {
	t.Helper()
	switch {
	case want == 0 && d.Set:
		t.Fatalf("the context has a deadline in %v, want none", d.Left)
	case want != 0 && !d.Set:
		t.Fatalf("the context has no deadline, want one in %v", want)
	case want != 0 && (d.Left > want || d.Left <= want-ObserveTimeout):
		t.Fatalf("the deadline is in %v, want about %v", d.Left, want)
	}
}

// DeadlineProbe is a model that answers "ok" and sends what its context had of
// a deadline to the returned channel.
func DeadlineProbe() (model *fakes.FunctionModel, deadlines <-chan Deadline) {
	seen := make(chan Deadline, 1)
	model = fakes.NewFunctionModel(func(
		ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		deadline, set := ctx.Deadline()
		seen <- Deadline{Left: time.Until(deadline), Set: set}
		return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: "ok"}}}, nil
	})
	return model, seen
}

// ToolCalls is a model that asks for one call of each named tool in its first
// response, so that the calls run concurrently, and answers "done" afterwards.
func ToolCalls(names ...string) *fakes.FunctionModel {
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
			parts[i] = ai.ToolCallPart{ToolName: name, Args: json.RawMessage(`{}`), ToolCallID: fmt.Sprintf("call-%d", i)}
		}
		return &ai.ModelResponse{Parts: parts}, nil
	})
}

// ConcurrentAgent calls the named tools concurrently. "explode" panics,
// "waiting" waits for its context, "fine" returns at once.
func ConcurrentAgent(names ...string) *ai.Agent[struct{}, string] {
	agent := ai.NewAgent[struct{}, string](ToolCalls(names...))
	ai.AddSimpleTool(agent, "explode", func(context.Context, struct{}) (string, error) {
		time.Sleep(20 * time.Millisecond) // lets "waiting" start first
		panic("kaboom in explode")
	})
	ai.AddSimpleTool(agent, "waiting", func(ctx context.Context, _ struct{}) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	ai.AddSimpleTool(agent, "fine", func(context.Context, struct{}) (string, error) {
		return "ok", nil
	})
	return agent
}

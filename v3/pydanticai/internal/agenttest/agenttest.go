// Package agenttest holds what the tests of this module share.
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

// ObserveTimeout bounds how long a test waits for something in the background.
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

func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// CaptureLog sends the log to the returned buffer; the test must not be parallel.
func CaptureLog(t testing.TB) *SyncBuffer {
	t.Helper()

	logs := new(SyncBuffer)
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return logs
}

func Answering(text string) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(context.Context, []ai.ModelMessage, ai.ModelRequestParams) (*ai.ModelResponse, error) {
		return &ai.ModelResponse{Parts: []ai.ResponsePart{ai.TextPart{Content: text}}}, nil
	})
}

func Failing(err error) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(context.Context, []ai.ModelMessage, ai.ModelRequestParams) (*ai.ModelResponse, error) {
		return nil, err
	})
}

func Panicking(value any) *fakes.FunctionModel {
	return fakes.NewFunctionModel(func(context.Context, []ai.ModelMessage, ai.ModelRequestParams) (*ai.ModelResponse, error) {
		panic(value)
	})
}

// Blocking is a model that waits until its context is canceled. started is closed
// when it is called, and canceled receives the cause.
func Blocking() (model *fakes.FunctionModel, started <-chan struct{}, canceled <-chan error) {
	startedCh := make(chan struct{})
	canceledCh := make(chan error, 1)
	var once sync.Once
	model = fakes.NewFunctionModel(func(
		ctx context.Context, _ []ai.ModelMessage, _ ai.ModelRequestParams,
	) (*ai.ModelResponse, error) {
		once.Do(func() { close(startedCh) })
		<-ctx.Done()
		select {
		case canceledCh <- context.Cause(ctx):
		default:
		}
		return nil, ctx.Err()
	})
	return model, startedCh, canceledCh
}

// Deadline is the time left to the deadline of a model call, or Set false.
type Deadline struct {
	Left time.Duration
	Set  bool
}

// Require fails the test unless the deadline is want away; zero means none.
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

// DeadlineProbe answers "ok" and sends the deadline of its context.
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

// ToolCalls asks for one call of each named tool at once, then answers "done".
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

// ConcurrentAgent calls the named tools together: "explode" and "aftershock"
// (after a cancel) panic, "waiting" waits for its context, "fine" returns.
func ConcurrentAgent(names ...string) *ai.Agent[struct{}, string] {
	agent := ai.NewAgent[struct{}, string](ToolCalls(names...))
	ai.AddSimpleTool(agent, "explode", func(context.Context, struct{}) (string, error) {
		time.Sleep(20 * time.Millisecond) // lets "waiting" start first
		panic("kaboom in explode")
	})
	ai.AddSimpleTool(agent, "aftershock", func(ctx context.Context, _ struct{}) (string, error) {
		<-ctx.Done()
		panic("kaboom in aftershock")
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

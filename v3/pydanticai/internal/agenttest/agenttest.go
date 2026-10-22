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

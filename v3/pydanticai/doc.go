// Package pydanticai provides Fiber v3 handlers for agents built with
// pydantic-ai-go (github.com/Kludex/pydantic-ai-go).
//
// AGUI and Vercel serve an agent to a chat frontend over the AG-UI and the
// Vercel AI SDK UI message stream protocols. Run serves an agent as a plain
// JSON endpoint. All three resolve the agent dependencies for each request from
// the Fiber context, and the streaming handlers cancel the agent run when the
// client goes away.
package pydanticai

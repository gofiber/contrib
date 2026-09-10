package socketio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/gofiber/utils/v2"
	"github.com/gofiber/utils/v2/simd"
)

// Wire-format helpers shared by the WebSocket and HTTP long-polling
// transports. Everything here is allocation-conscious: outbound frames are
// built into a single pre-sized buffer, JSON strings are encoded with
// gofiber/utils' SWAR encoder instead of encoding/json's reflection path,
// and inbound packets are split into sub-slices of the frame they arrived
// in rather than copied.

var (
	errNotJSONArray       = errors.New("socketio: payload is not a JSON array")
	errEventNameNotString = errors.New("socketio: event name is not a JSON string")
)

// buildEIOOpenFrame returns the full Engine.IO OPEN frame
// (`0{"sid":...,"upgrades":[],"pingInterval":N,"pingTimeout":N,"maxPayload":N}`)
// for a freshly opened session. The empty "upgrades" array signals "no
// transport upgrade available" per Engine.IO v4. The frame is assembled in
// one allocation; the error return is kept for API stability and is always
// nil.
func buildEIOOpenFrame(sid string) ([]byte, error) {
	maxPayload := MaxPayload
	if maxPayload <= 0 {
		maxPayload = defaultMaxPayload
	}
	buf := make([]byte, 0, 96+len(sid))
	buf = append(buf, eioOpen)
	buf = append(buf, `{"sid":`...)
	buf = utils.AppendJSONString(buf, sid)
	buf = append(buf, `,"upgrades":[],"pingInterval":`...)
	buf = strconv.AppendInt(buf, PingInterval.Milliseconds(), 10)
	buf = append(buf, `,"pingTimeout":`...)
	buf = strconv.AppendInt(buf, PingTimeout.Milliseconds(), 10)
	buf = append(buf, `,"maxPayload":`...)
	buf = strconv.AppendInt(buf, maxPayload, 10)
	buf = append(buf, '}')
	return buf, nil
}

// appendSIOPrefix appends the "4<type>[/<ns>,]" head shared by every
// Socket.IO packet.
func appendSIOPrefix(buf []byte, sioType byte, namespace []byte) []byte {
	buf = append(buf, eioMessage, sioType)
	if len(namespace) > 0 {
		buf = append(buf, namespace...)
		buf = append(buf, ',')
	}
	return buf
}

// buildSIOConnectAckSID encodes the CONNECT ack the server sends once the
// namespace/auth handshake succeeded: `40[/ns,]{"sid":"<sid>"}`.
func buildSIOConnectAckSID(namespace []byte, sid string) []byte {
	buf := make([]byte, 0, 13+len(namespace)+len(sid))
	buf = appendSIOPrefix(buf, sioConnect, namespace)
	buf = append(buf, `{"sid":`...)
	buf = utils.AppendJSONString(buf, sid)
	return append(buf, '}')
}

// buildSIOConnectError encodes a SIO CONNECT_ERROR frame
// ("44[/ns,]<json>"). jsonMessage must already be a JSON object such as
// `{"message":"Not authorized"}` (socket.io-protocol v5).
func buildSIOConnectError(namespace []byte, jsonMessage string) []byte {
	buf := make([]byte, 0, 3+len(namespace)+len(jsonMessage))
	buf = appendSIOPrefix(buf, sioConnectError, namespace)
	return append(buf, jsonMessage...)
}

// buildSIODisconnect encodes a SIO DISCONNECT frame. Per socket.io-protocol
// v5 a namespaced packet is "41/<ns>," with a trailing comma separating the
// namespace from the (empty) payload.
func buildSIODisconnect(namespace []byte) []byte {
	buf := make([]byte, 0, 3+len(namespace))
	return appendSIOPrefix(buf, sioDisconnect, namespace)
}

// buildSIOEvent encodes a Socket.IO EVENT packet ready to send over the wire.
// Format: 4 2 [/<namespace>,] [ "<event>" , <data> ]
//
// data may be valid JSON (object, array, string, number, etc.) or raw text.
// Raw text is encoded as a JSON string for compatibility with earlier
// versions that accepted arbitrary bytes in Emit.
// namespace may be nil for the root namespace.
func buildSIOEvent(namespace []byte, event string, data []byte) []byte {
	if len(data) == 0 {
		return buildSIOEventWithAck(namespace, 0, false, event, nil)
	}
	return buildSIOEventWithAck(namespace, 0, false, event, [][]byte{data})
}

// buildSIOEventWithAck is the ack-id aware multi-arg variant of buildSIOEvent.
//
// args is the slice of arguments to encode after the event name (matches the
// JS-side socket.emit("event", a, b, c) shape). Entries that are valid JSON are
// passed through unchanged; raw text entries are encoded as JSON strings.
// Nil/empty entries are skipped.
//
// The buffer is sized up front so a typical event costs exactly one
// allocation; only an event name or raw-text argument that needs escaping
// can push it past the estimate.
func buildSIOEventWithAck(namespace []byte, ackID uint64, hasAck bool, event string, args [][]byte) []byte {
	size := 2 + len(namespace) + 1 + 20 + 1 + len(event) + 2 + 1
	for _, a := range args {
		size += len(a) + 1
	}
	buf := make([]byte, 0, size)
	buf = appendSIOPrefix(buf, sioEvent, namespace)
	if hasAck {
		buf = strconv.AppendUint(buf, ackID, 10)
	}
	buf = append(buf, '[')
	buf = utils.AppendJSONString(buf, event)
	for _, a := range args {
		if len(a) == 0 {
			continue
		}
		buf = append(buf, ',')
		buf = appendJSONArg(buf, a)
	}
	return append(buf, ']')
}

// buildSIOAck encodes a Socket.IO ACK ("43") packet.
//
// Format: 4 3 [/<namespace>,] <ackID> [ <args> ]
// args may be nil/empty to send `43<id>[]`. Valid JSON args are passed
// through; raw text args are encoded as JSON strings.
func buildSIOAck(namespace []byte, ackID uint64, args [][]byte) []byte {
	size := 2 + len(namespace) + 1 + 20 + 2
	for _, a := range args {
		size += len(a) + 1
	}
	buf := make([]byte, 0, size)
	buf = appendSIOPrefix(buf, sioAck, namespace)
	buf = strconv.AppendUint(buf, ackID, 10)
	buf = append(buf, '[')
	first := true
	for _, a := range args {
		if len(a) == 0 {
			continue
		}
		if !first {
			buf = append(buf, ',')
		}
		buf = appendJSONArg(buf, a)
		first = false
	}
	return append(buf, ']')
}

// appendJSONArg appends one emit argument: valid JSON is copied verbatim,
// anything else is encoded as a JSON string (byte-identical to
// encoding/json.Marshal of the same text, HTML escaping included) using the
// SWAR encoder from gofiber/utils. Plain text is recognised by its first
// byte before json.Valid runs, which would otherwise allocate a syntax
// error for every raw-text argument.
func appendJSONArg(buf, data []byte) []byte {
	if mayBeJSON(data) && json.Valid(data) {
		return append(buf, data...)
	}
	return utils.AppendJSONString(buf, data)
}

// mayBeJSON reports whether data starts, after optional whitespace, with a
// byte that can begin a JSON value. Anything else is raw text for sure.
func mayBeJSON(data []byte) bool {
	for _, c := range data {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[', '"', '-', 't', 'f', 'n', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			return true
		default:
			return false
		}
	}
	return false
}

// splitSIOAckID extracts the optional leading numeric ack ID from a SIO
// EVENT or ACK payload (the bytes after any namespace stripping). It
// returns the ID, a flag whether one was present, and the remaining bytes.
//
// Hand-rolled digit accumulation avoids the string allocation that
// strconv.ParseUint(string(data[:i])) would force on the hot inbound path.
func splitSIOAckID(data []byte) (id uint64, has bool, rest []byte, err error) {
	var v uint64
	i := 0
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		d := uint64(data[i] - '0')
		if v > (math.MaxUint64-d)/10 {
			return 0, false, data, ErrAckIDOverflow
		}
		v = v*10 + d
		i++
	}
	if i == 0 {
		return 0, false, data, nil
	}
	return v, true, data[i:], nil
}

// parseSIOEvent parses the JSON-array payload of a Socket.IO EVENT packet.
//
// payload is the bytes after the "42" prefix, e.g. `["message",{"key":"val"}]`.
// It returns the event name and a slice of raw-JSON arguments (one entry
// per element after the event name). args is nil for events without
// arguments.
//
// The arguments are sub-slices of payload, not copies: both transports hand
// the parser a buffer that belongs to the connection (the WebSocket
// ReadMessage returns a fresh buffer per message and the polling handler
// copies the request body once), so listeners may retain them freely.
func parseSIOEvent(payload []byte) (string, [][]byte, error) {
	// Name plus up to three arguments fit the first allocation.
	elems, err := splitJSONArray(payload, make([][]byte, 0, 4))
	if err != nil {
		return "", nil, fmt.Errorf("socketio: failed to parse event payload: %w", err)
	}
	if len(elems) == 0 {
		return "", nil, ErrEmptyEventArray
	}
	name, err := unquoteJSONString(elems[0])
	if err != nil {
		return "", nil, fmt.Errorf("socketio: failed to parse event name: %w", err)
	}
	if MaxEventNameLength > 0 && len(name) > MaxEventNameLength {
		return "", nil, fmt.Errorf("socketio: event name exceeds MaxEventNameLength (%d)", MaxEventNameLength)
	}
	if len(elems) == 1 {
		return name, nil, nil
	}
	return name, elems[1:], nil
}

// parseSIOAckArgs parses the JSON-array payload of a Socket.IO ACK packet
// (the bytes after the ack id). Returns nil for an empty array.
func parseSIOAckArgs(payload []byte) ([][]byte, error) {
	elems, err := splitJSONArray(payload, nil)
	if err != nil {
		return nil, err
	}
	if len(elems) == 0 {
		return nil, nil
	}
	return elems, nil
}

// splitJSONArray appends the top-level elements of the JSON array payload
// to dst as sub-slices of payload and returns the extended slice. Leading
// and trailing whitespace around the array and around each element is
// dropped. It returns an error when payload is not a syntactically valid
// JSON array, or ErrTooManyArgs once more than MaxEventArgs elements would
// be appended.
//
// The payload is validated once with encoding/json's allocation-free
// scanner, after which only structure matters: nesting depth and string
// bodies. String bodies, which dominate typical payloads, are skipped with
// utils.IndexAny2 (SIMD on amd64 for 32+ byte runs, SWAR otherwise) rather
// than byte by byte.
func splitJSONArray(payload []byte, dst [][]byte) ([][]byte, error) {
	p := trimJSONSpace(payload)
	if len(p) < 2 || p[0] != '[' || p[len(p)-1] != ']' || !json.Valid(p) {
		return nil, errNotJSONArray
	}
	body := p[1 : len(p)-1]
	limit := MaxEventArgs
	appended := 0
	i := 0
	for i < len(body) {
		for i < len(body) && isJSONSpace(body[i]) {
			i++
		}
		if i >= len(body) {
			break
		}
		if limit > 0 && appended >= limit {
			return nil, ErrTooManyArgs
		}
		start := i
		depth := 0
	scan:
		for i < len(body) {
			switch body[i] {
			case '"':
				i = skipJSONString(body, i+1)
				continue
			case '[', '{':
				depth++
			case ']', '}':
				depth--
			case ',':
				if depth == 0 {
					break scan
				}
			}
			i++
		}
		end := i
		for end > start && isJSONSpace(body[end-1]) {
			end--
		}
		dst = append(dst, body[start:end:end])
		appended++
		i++ // past the separating comma, or past the end
	}
	return dst, nil
}

// skipJSONString returns the index just past the closing quote of the JSON
// string whose body starts at i (the opening quote already consumed). A
// backslash escapes the byte after it, so a scan for either byte never
// stops inside an escape sequence.
func skipJSONString(b []byte, i int) int {
	for i < len(b) {
		j := utils.IndexAny2(b[i:], '"', '\\')
		if j < 0 {
			return len(b)
		}
		if b[i+j] == '\\' {
			i += j + 2
			continue
		}
		return i + j + 1
	}
	return i
}

// unquoteJSONString decodes a JSON string literal. Literals without escape
// sequences and with valid UTF-8, the overwhelmingly common case for event
// names, are converted with a single string allocation; the rest go
// through encoding/json.
func unquoteJSONString(elem []byte) (string, error) {
	if len(elem) < 2 || elem[0] != '"' || elem[len(elem)-1] != '"' {
		return "", errEventNameNotString
	}
	body := elem[1 : len(elem)-1]
	// Without escapes the bytes are the string, provided they are valid
	// UTF-8: json.Valid does not check UTF-8 inside strings, and
	// encoding/json decodes each invalid byte as U+FFFD, so such names take
	// the decoder path below and come out as the escaped path decodes them.
	if bytes.IndexByte(body, '\\') < 0 && utf8.Valid(body) {
		return string(body), nil
	}
	var s string
	if err := json.Unmarshal(elem, &s); err != nil {
		return "", err
	}
	return s, nil
}

// isJSONSpace reports whether c is insignificant whitespace per RFC 8259.
func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// trimJSONSpace strips RFC 8259 whitespace from both ends of b.
func trimJSONSpace(b []byte) []byte {
	for len(b) > 0 && isJSONSpace(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && isJSONSpace(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

// extractSIOConnect parses the bytes after the "40" type prefix of a SIO
// CONNECT packet and returns both the optional namespace (including the
// leading "/", or nil for root) AND the optional JSON auth payload.
//
// Wire format: [ "/" namespace "," ] [ <json> ]
//
// Examples:
//
//	""                   -> (nil, nil)
//	"{"token":"x"}"      -> (nil, `{"token":"x"}`)
//	"/admin,"            -> ("/admin", nil)
//	"/admin,{"k":1}"     -> ("/admin", `{"k":1}`)
//	"/admin"             -> ("/admin", nil)   // no comma, no auth
func extractSIOConnect(data []byte) (namespace, auth []byte) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[0] != '/' {
		return nil, data
	}
	ns, rest, found := utils.CutByte(data, ',')
	if !found {
		// Namespace without trailing comma (no auth payload).
		return data, nil
	}
	if len(rest) == 0 {
		return ns, nil
	}
	return ns, rest
}

// splitSIONamespace separates an optional "/<ns>," prefix from a packet
// payload. Returns nil and data unchanged when no prefix is present.
func splitSIONamespace(data []byte) (namespace, rest []byte) {
	if len(data) == 0 || data[0] != '/' {
		return nil, data
	}
	ns, rest, found := utils.CutByte(data, ',')
	if !found {
		return data, nil
	}
	return ns, rest
}

// isValidNamespace returns true when ns matches the conservative subset of
// the socket.io namespace grammar: empty (root), or "/<segment>" with at
// least one byte and only [A-Za-z0-9._\-/] characters. We deliberately
// reject characters that would change framing if echoed verbatim into a
// "42<ns>,..." event packet.
//
// Runs of word characters are skipped with simd.MemchrNotWord (AVX2 on
// amd64 for 32+ byte inputs, SWAR otherwise); only the three extra
// punctuation bytes are checked one at a time.
func isValidNamespace(ns []byte) bool {
	if len(ns) == 0 {
		return true
	}
	if ns[0] != '/' {
		return false
	}
	// A lone "/" is treated as the root namespace by socket.io and is
	// accepted here.
	rest := ns[1:]
	for len(rest) > 0 {
		i := simd.MemchrNotWord(rest)
		if i < 0 {
			return true
		}
		switch rest[i] {
		case '_', '-', '.', '/':
			rest = rest[i+1:]
		default:
			return false
		}
	}
	return true
}

// isValidAuthPayload returns true when the auth payload extracted from a
// SIO CONNECT packet conforms to socket.io-protocol v5: either absent (nil
// / empty), or a syntactically valid JSON object (i.e. first non-whitespace
// byte is '{'). Arrays, scalars, strings, and malformed JSON are rejected.
//
// Also enforces the global MaxAuthPayload cap so an oversized auth payload
// can never reach kws.handshakeAuth or user-visible state.
func isValidAuthPayload(auth []byte) bool {
	if len(auth) == 0 {
		return true
	}
	if MaxAuthPayload > 0 && len(auth) > MaxAuthPayload {
		return false
	}
	trimmed := trimJSONSpace(auth)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	return json.Valid(trimmed)
}

// isReservedEventName reports whether name is a reserved socket.io
// lifecycle event name that must not be used as a custom event name.
//
// Only wire-level reserved names are blocked. Node EventEmitter internals
// (disconnecting, newListener, removeListener) never appear on the wire and
// must not constrain user event names.
func isReservedEventName(name string) bool {
	switch name {
	case "connect", "connect_error", "disconnect":
		return true
	}
	return false
}

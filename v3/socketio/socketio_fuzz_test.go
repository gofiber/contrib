package socketio

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"unicode/utf8"
)

// newFuzzKws builds a parser-only Websocket: Conn nil, isAlive false,
// so write() no-ops and handleSIOPacket cannot reach the wire.
func newFuzzKws() *Websocket {
	return &Websocket{queue: make(chan message, 100), done: make(chan struct{}, 1), attributes: make(map[string]interface{}), outboundAcks: make(map[uint64]*pendingAck)}
}

// FuzzHandleSIOPacket: arbitrary bytes through the SIO dispatcher.
// Catches panics in ns stripping, ack-id parsing, JSON unmarshal of
// malformed payloads, and unknown SIO type handling.
func FuzzHandleSIOPacket(f *testing.F) {
	for _, s := range [][]byte{
		nil, {}, {'0'}, {'1'}, {'2'}, {'3'}, {'4'}, {'5'}, {'6'}, {'9'},
		[]byte(`2["msg","hi"]`), []byte(`2/admin,["msg",1]`),
		[]byte(`212["msg",{}]`), []byte(`2/ns,7["evt",[1,2,3]]`),
		[]byte(`2["evt"`), []byte(`2/admin`), []byte(`2/admin,`),
		[]byte(`299999999999999999999999["x"]`), []byte(`30[]`),
		[]byte(`3/ns,5[{"k":1}]`), []byte(`0/admin,{"token":"x"}`),
		[]byte(`1/admin`),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		kws := newFuzzKws()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("handleSIOPacket panic on %q: %v", payload, r)
			}
		}()
		kws.handleSIOPacket(payload)
	})
}

// FuzzExtractSIOConnect: random bytes through extractSIOConnect.
// Catches OOB slicing and prefix confusion. Invariants: non-empty ns
// starts with '/'; auth is nil ("no auth") or non-empty.
func FuzzExtractSIOConnect(f *testing.F) {
	for _, s := range []string{
		"", "{}", "/", "/a", "/a,", "/admin,{\"t\":1}", "/admin",
		",", "/a,b,c", "{\"k\":\"/v\"}", "/a/b/c,",
		"\x00", "/\x00,", "noisy", "/" + string([]byte{0xff, 0xfe}),
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("extractSIOConnect panic on %q: %v", data, r)
			}
		}()
		ns, auth := extractSIOConnect(data)
		if len(ns) > 0 && ns[0] != '/' {
			t.Fatalf("ns must start with '/': got %q (in %q)", ns, data)
		}
		if auth != nil && len(auth) == 0 {
			t.Fatalf("auth must be nil or non-empty for %q", data)
		}
	})
}

// FuzzSplitSIOAckID: random digit-prefixed strings. Catches uint64
// overflow, leading-zero edge cases, slice arithmetic. Invariants:
// valid digit runs yield has=true with the matching id; overflow
// returns err != nil and has=false.
func FuzzSplitSIOAckID(f *testing.F) {
	for _, s := range []string{
		"", "0", "1", "42", "0042", "[", "12[", "1[\"x\"]",
		"99999999999999999999999999",
		strconv.FormatUint(^uint64(0), 10),
		strconv.FormatUint(^uint64(0), 10) + "0",
		"18446744073709551616",
		"abc", "1a", "12 ", "9223372036854775807rest",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("splitSIOAckID panic on %q: %v", data, r)
			}
		}()
		id, has, rest, err := splitSIOAckID(data)
		n := 0
		for n < len(data) && data[n] >= '0' && data[n] <= '9' {
			n++
		}
		if n == 0 {
			if has || err != nil || id != 0 || string(rest) != string(data) {
				t.Fatalf("no-digit must yield (0,false,data,nil), got id=%d has=%v err=%v", id, has, err)
			}
			return
		}
		if _, perr := strconv.ParseUint(string(data[:n]), 10, 64); perr != nil {
			if err == nil {
				t.Fatalf("expected err on overflow %q, got id=%d has=%v", data[:n], id, has)
			}
			if has {
				t.Fatalf("overflow must report has=false for %q", data[:n])
			}
			return
		}
		if !has || err != nil {
			t.Fatalf("valid %q produced has=%v err=%v", data[:n], has, err)
		}
		if string(rest) != string(data[n:]) {
			t.Fatalf("rest mismatch: want %q got %q", data[n:], rest)
		}
	})
}

// FuzzParseSIOEvent: arbitrary bytes to the JSON-array event parser.
// Catches panics on malformed JSON and accidental name returns when
// outer JSON is invalid. Invariant: a name is returned only when err
// is nil AND the payload is valid JSON.
func FuzzParseSIOEvent(f *testing.F) {
	for _, s := range []string{
		`["evt"]`, `["evt",1]`, `["evt",{"k":1}]`, `["evt",1,2,3]`,
		`[]`, `[1]`, `[null]`, `["x", "y"]`,
		`not json`, `{`, `[`, `["unterminated`,
		`[{"k":1}]`, `[true,false]`, `["evt",` + string([]byte{0xff}) + `]`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("parseSIOEvent panic on %q: %v", payload, r)
			}
		}()
		name, data, err := parseSIOEvent(payload)
		if err != nil {
			if name != "" || data != nil {
				t.Fatalf("on err, name/data must be zero: name=%q data=%q", name, data)
			}
			return
		}
		if !json.Valid(payload) {
			t.Fatalf("success on invalid JSON %q", payload)
		}
		if !utf8.ValidString(name) {
			t.Fatalf("name not valid UTF-8: %q (in %q)", name, payload)
		}
	})
}

// FuzzReadFrame mirrors read()'s post-ReadMessage decision tree.
// We do not modify socketio.go to extract a helper; the switch here
// is a faithful copy so fuzz can exercise the parser path without a
// real conn or goroutines. Catches panics in handleSIOPacket under
// arbitrary EIO bytes and crashes on empty / non-text frames.
func FuzzReadFrame(f *testing.F) {
	type s struct {
		mType int
		msg   []byte
	}
	for _, c := range []s{
		{TextMessage, []byte("2probe")}, {TextMessage, []byte("3probe")},
		{TextMessage, []byte("0{}")}, {TextMessage, []byte("1")},
		{TextMessage, []byte(`42["msg","hi"]`)},
		{TextMessage, []byte(`43/admin,7[{"ok":true}]`)},
		{TextMessage, []byte(`40/admin,`)},
		{TextMessage, []byte("5")}, {TextMessage, []byte("6")},
		{TextMessage, nil}, {TextMessage, []byte{}},
		{BinaryMessage, []byte{0x00, 0x01, 0x02}},
		{PingMessage, nil}, {PongMessage, nil}, {CloseMessage, nil},
		{TextMessage, []byte{0xff}},
	} {
		f.Add(c.mType, c.msg)
	}
	f.Fuzz(func(t *testing.T, mType int, msg []byte) {
		kws := newFuzzKws()
		kws.lastPongNanos.Store(int64(1))
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("dispatch panic on (%d,%q): %v", mType, msg, r)
			}
		}()
		switch mType {
		case PingMessage, PongMessage, CloseMessage, BinaryMessage:
			return
		}
		if mType != TextMessage || len(msg) == 0 {
			return
		}
		if msg[0] == eioMessage {
			kws.handleSIOPacket(msg[1:])
		}
	})
}

// FuzzExtractSIONamespace: arbitrary bytes through the namespace splitter.
// Invariants: never panics; returned ns is either nil or has the same byte
// content as data[:len(ns)] (i.e., a sub-slice of the input).
func FuzzExtractSIONamespace(f *testing.F) {
	for _, s := range [][]byte{
		nil, {}, []byte("/admin,"), []byte("/a/b/c,{\"k\":1}"),
		[]byte("/admin"), []byte("noprefix"), []byte("{\"a\":1}"),
		append([]byte("/"), bytes.Repeat([]byte("a"), 4096)...), // oversize
		[]byte("/\x00,"), []byte(",,,"),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("extractSIONamespace panic on %q: %v", data, r)
			}
		}()
		ns := extractSIONamespace(data)
		if ns == nil {
			return
		}
		if len(ns) > len(data) || !bytes.Equal(ns, data[:len(ns)]) {
			t.Fatalf("ns must be sub-slice of data: ns=%q data=%q", ns, data)
		}
		if ns[0] != '/' {
			t.Fatalf("non-nil ns must start with '/': %q", ns)
		}
	})
}

// FuzzIsValidNamespace: arbitrary bytes through the namespace validator.
// Invariants: never panics; result is deterministic (idempotent on same input).
func FuzzIsValidNamespace(f *testing.F) {
	for _, s := range [][]byte{
		nil, {}, []byte("/"), []byte("/a"), []byte("/admin"),
		[]byte("/a/b.c-_"), []byte("admin"), []byte("/a,b"),
		[]byte("/\x00"), []byte("/\r\n"), bytes.Repeat([]byte("a"), 8192), // oversize
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, ns []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("isValidNamespace panic on %q: %v", ns, r)
			}
		}()
		a := isValidNamespace(ns)
		b := isValidNamespace(ns)
		if a != b {
			t.Fatalf("non-deterministic on %q: %v vs %v", ns, a, b)
		}
	})
}

// FuzzBuildSIOEventWithAck: round-trip property. Build a "42<ns>,<id>[name,data]"
// frame, strip EIO+SIO prefix and namespace, then parseSIOEvent must recover
// the same name and the same single-arg JSON data.
func FuzzBuildSIOEventWithAck(f *testing.F) {
	for _, c := range []struct {
		ns, name string
		data     []byte
	}{
		{"", "msg", []byte(`"hi"`)}, {"/admin", "evt", []byte(`{"k":1}`)},
		{"/a/b", "x", []byte(`[1,2,3]`)}, {"", "n", []byte(`null`)},
		{"", "empty", nil},                            // empty data
		{"/bad,ns", "evt", []byte(`1`)},               // malformed ns (will be rejected)
		{"", string(make([]byte, 1024)), []byte(`1`)}, // oversize name (will be rejected)
	} {
		f.Add([]byte(c.ns), c.name, c.data)
	}
	f.Fuzz(func(t *testing.T, ns []byte, name string, data []byte) {
		if !isValidNamespace(ns) || !utf8.ValidString(name) {
			return
		}
		if len(name) > MaxEventNameLength || isReservedEventName(name) {
			return
		}
		if len(data) > 0 && !json.Valid(data) {
			return
		}
		var args [][]byte
		if len(data) > 0 {
			args = [][]byte{data}
		}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("build/parse panic ns=%q name=%q data=%q: %v", ns, name, data, r)
			}
		}()
		frame := buildSIOEventWithAck(ns, 7, true, name, args)
		if len(frame) < 2 || frame[0] != eioMessage || frame[1] != sioEvent {
			t.Fatalf("frame missing EIO+SIO prefix: %q", frame)
		}
		body := frame[2:]
		if len(ns) > 0 {
			if !bytes.HasPrefix(body, ns) || len(body) <= len(ns) || body[len(ns)] != ',' {
				t.Fatalf("namespace not echoed: body=%q ns=%q", body, ns)
			}
			body = body[len(ns)+1:]
		}
		_, _, rest, err := splitSIOAckID(body)
		if err != nil {
			t.Fatalf("split ack id: %v on %q", err, body)
		}
		gotName, gotArgs, err := parseSIOEvent(rest)
		if err != nil {
			t.Fatalf("parseSIOEvent err on %q: %v (frame=%q)", rest, err, frame)
		}
		if gotName != name {
			t.Fatalf("name mismatch: want %q got %q", name, gotName)
		}
		if len(args) == 0 {
			if len(gotArgs) != 0 {
				t.Fatalf("expected zero args, got %d", len(gotArgs))
			}
			return
		}
		if len(gotArgs) != 1 {
			t.Fatalf("want 1 arg got %d (%q)", len(gotArgs), gotArgs)
		}
		// Compare canonical JSON: parser whitespace stripping is semantic
		// no-op, so we round-trip both sides through json.Compact.
		var gotBuf, wantBuf bytes.Buffer
		if err := json.Compact(&gotBuf, gotArgs[0]); err != nil {
			t.Fatalf("compact got: %v", err)
		}
		if err := json.Compact(&wantBuf, data); err != nil {
			t.Fatalf("compact want: %v", err)
		}
		if !bytes.Equal(gotBuf.Bytes(), wantBuf.Bytes()) {
			t.Fatalf("data mismatch: want %q got %q", wantBuf.Bytes(), gotBuf.Bytes())
		}
	})
}

// FuzzBuildSIOAck: round-trip property for "43" ACK frames. After stripping
// EIO+SIO prefix and namespace, splitSIOAckID must yield the same id and the
// JSON tail must contain the same data.
func FuzzBuildSIOAck(f *testing.F) {
	oversize := append(bytes.Repeat([]byte(`"a",`), (1<<10)-1), '"', 'a', '"')
	for _, c := range []struct {
		ns   string
		id   uint64
		data []byte
	}{
		{"", 0, []byte(`"ok"`)}, {"/admin", 42, []byte(`{"x":1}`)},
		{"", 1, nil}, {"/n", ^uint64(0), []byte(`[1,2]`)},
		{"", 0, []byte("not json")}, // malformed (will be rejected)
		{"", 0, nil},                 // empty
		{"", 7, append([]byte("["), append(oversize, ']')...)}, // oversize valid JSON arg
	} {
		f.Add([]byte(c.ns), c.id, c.data)
	}
	f.Fuzz(func(t *testing.T, ns []byte, id uint64, data []byte) {
		if !isValidNamespace(ns) {
			return
		}
		if len(data) > 0 && !json.Valid(data) {
			return
		}
		var args [][]byte
		if len(data) > 0 {
			args = [][]byte{data}
		}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("buildSIOAck panic ns=%q id=%d data=%q: %v", ns, id, data, r)
			}
		}()
		frame := buildSIOAck(ns, id, args)
		if len(frame) < 2 || frame[0] != eioMessage || frame[1] != sioAck {
			t.Fatalf("missing EIO+SIO prefix: %q", frame)
		}
		body := frame[2:]
		if len(ns) > 0 {
			if !bytes.HasPrefix(body, ns) || len(body) <= len(ns) || body[len(ns)] != ',' {
				t.Fatalf("ns not echoed: body=%q ns=%q", body, ns)
			}
			body = body[len(ns)+1:]
		}
		gotID, has, rest, err := splitSIOAckID(body)
		if err != nil || !has || gotID != id {
			t.Fatalf("ack id mismatch: want %d got %d has=%v err=%v body=%q", id, gotID, has, err, body)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(rest, &arr); err != nil {
			t.Fatalf("ack tail not JSON array: %q: %v", rest, err)
		}
		if len(args) == 0 {
			if len(arr) != 0 {
				t.Fatalf("want empty arr got %d", len(arr))
			}
			return
		}
		if len(arr) != 1 {
			t.Fatalf("want 1 arg got %d (%q)", len(arr), arr)
		}
		var gotBuf, wantBuf bytes.Buffer
		if err := json.Compact(&gotBuf, arr[0]); err != nil {
			t.Fatalf("compact got: %v", err)
		}
		if err := json.Compact(&wantBuf, data); err != nil {
			t.Fatalf("compact want: %v", err)
		}
		if !bytes.Equal(gotBuf.Bytes(), wantBuf.Bytes()) {
			t.Fatalf("data mismatch: want %q got %q", wantBuf.Bytes(), gotBuf.Bytes())
		}
	})
}

// FuzzBatchedEIOFrame: drive the read-loop's 0x1E batch scanner with arbitrary
// bytes. Replicates the loop in read() faithfully, dispatching synchronously
// (no goroutines spawned per packet). Asserts: no panic, the dispatched count
// is bounded by MaxBatchPackets, and every packet aliases the original frame.
func FuzzBatchedEIOFrame(f *testing.F) {
	for _, s := range [][]byte{
		nil, {}, []byte("2probe"),
		[]byte("2probe\x1e3probe"),
		[]byte("\x1e\x1e\x1e"),
		bytes.Repeat([]byte{0x1E}, 1024),                        // adversarial separators
		append([]byte("4"), bytes.Repeat([]byte("x"), 4096)...), // oversize single packet
		[]byte("4" + `2["msg",1]` + "\x1e" + "4" + `2["msg",2]`),
		[]byte("malformed\x1e"),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, msg []byte) {
		kws := newFuzzKws()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("batch parse panic on %q: %v", msg, r)
			}
		}()
		if len(msg) == 0 {
			return
		}
		// dispatchSafe skips packets whose handler would touch kws.Conn
		// (which is nil in this parser-only harness). The harness goal
		// is to fuzz the batch SCANNER, not re-cover the per-packet
		// handlers (already covered by FuzzHandleSIOPacket).
		dispatchSafe := func(p []byte) {
			if len(p) >= 2 && p[0] == eioMessage && p[1] == sioConnect {
				return
			}
			kws.dispatchEIOPacket(p)
		}
		if bytes.IndexByte(msg, 0x1E) < 0 {
			dispatchSafe(msg)
			return
		}
		rest, count := msg, 0
		for len(rest) > 0 {
			if count > MaxBatchPackets {
				kws.fireEvent(EventError, nil, errors.New("test: exceeds MaxBatchPackets"))
				break
			}
			idx := bytes.IndexByte(rest, 0x1E)
			var packet []byte
			if idx < 0 {
				packet, rest = rest, nil
			} else {
				packet, rest = rest[:idx], rest[idx+1:]
			}
			if len(packet) == 0 {
				continue
			}
			// Aliasing invariant: packet[0] must point inside msg's backing
			// array. cap(packet) is bounded by cap(msg) when packet is a
			// sub-slice; this catches the case where the loop accidentally
			// allocates a fresh slice instead of reusing the input.
			if cap(packet) > cap(msg) {
				t.Fatalf("packet escaped msg backing array: cap(p)=%d cap(msg)=%d", cap(packet), cap(msg))
			}
			dispatchSafe(packet)
			count++
		}
		if count > MaxBatchPackets+1 {
			t.Fatalf("dispatched %d > MaxBatchPackets %d", count, MaxBatchPackets)
		}
	})
}

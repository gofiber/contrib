package socketio

import (
	"encoding/json"
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

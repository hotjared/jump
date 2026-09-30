package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestScreenClipboardRoundTripBoundariesAndUnicode(t *testing.T) {
	for _, text := range []string{"", "héllo 世界 😀", strings.Repeat("a", screenClipboardMax), strings.Repeat("a", screenChunkBytes-1) + "😀"} {
		for _, kind := range []string{"clipboard_set", "clipboard_get"} {
			var op screenOperation
			if op.request(message{Type: "screen_operation", RequestID: screenTestID, Kind: kind}) != nil {
				t.Fatal("request")
			}
			for _, chunk := range clipboardChunks(screenTestID, []byte(text)) {
				ack := message{Type: "screen_clipboard_ack", RequestID: screenTestID, Index: chunk.Index}
				var err, errorAck error
				if kind == "clipboard_set" {
					err = op.request(chunk)
					errorAck = op.response(ack)
				} else {
					err = op.response(chunk)
					errorAck = op.request(ack)
				}
				if err != nil || errorAck != nil {
					t.Fatal("chunk/ack", err, errorAck)
				}
			}
			if string(op.data) != text {
				t.Fatal("text differs")
			}
			if op.response(message{Type: "screen_operation_result", RequestID: screenTestID, Kind: kind, Code: "ok"}) != nil || op.data != nil {
				t.Fatal("completion did not clear text")
			}
		}
	}
}
func TestScreenClipboardRejectsMalformedAndIncomplete(t *testing.T) {
	request := message{Type: "screen_operation", RequestID: screenTestID, Kind: "clipboard_set"}
	chunk := message{Type: "screen_clipboard", RequestID: screenTestID, Count: 1, Data: base64.StdEncoding.EncodeToString([]byte("text"))}
	for _, mutation := range []func(*message){
		func(m *message) { m.Index = 1 }, func(m *message) { m.Count = 65 }, func(m *message) { m.Data = "!" },
		func(m *message) {
			m.Data = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), screenChunkBytes+1))
		},
		func(m *message) { m.Data = base64.StdEncoding.EncodeToString([]byte{255}) },
		func(m *message) { m.Data = base64.StdEncoding.EncodeToString([]byte("x\x00y")) },
		func(m *message) { m.RequestID = "wrong" }, func(m *message) { m.Count = 2 },
	} {
		var op screenOperation
		_ = op.request(request)
		bad := chunk
		mutation(&bad)
		if op.request(bad) == nil {
			t.Fatal("accepted malformed chunk")
		}
	}
	var op screenOperation
	_ = op.request(request)
	_ = op.request(chunk)
	if op.request(chunk) == nil {
		t.Fatal("duplicate accepted")
	}
	if op.response(message{Type: "screen_operation_result", RequestID: screenTestID, Kind: "clipboard_set", Code: "ok"}) == nil {
		t.Fatal("missing acknowledgement accepted")
	}
	if clipboardText(bytes.Repeat([]byte("x"), screenClipboardMax+1)) {
		t.Fatal("oversize accepted")
	}
}
func TestScreenEventsAndErrorsAreAllowlisted(t *testing.T) {
	for _, name := range []string{"Default", "Winlogon", "ScreenSaver"} {
		if safeDesktopName(name) != name || !safeScreenEvent(message{Stage: "desktop_changed", Desktop: name}) {
			t.Fatal(name)
		}
	}
	for _, name := range []string{"secret arbitrary OS name", "path\\name"} {
		if safeDesktopName(name) != "" || safeScreenEvent(message{Stage: "desktop_changed", Desktop: name}) {
			t.Fatal("name leaked")
		}
	}
	if safeOperationCode("clipboard contains secrets") || safeScreenEvent(message{Stage: "clipboard_contents"}) {
		t.Fatal("unsafe diagnostics")
	}
}

func TestCancellationAllowsInFlightRepliesUntilSafeCompletion(t *testing.T) {
	var op screenOperation
	_ = op.request(message{Type: "screen_operation", RequestID: screenTestID, Kind: "clipboard_get"})
	if op.request(message{Type: "screen_operation_cancel", RequestID: screenTestID}) != nil || !op.cancelled {
		t.Fatal("cancel")
	}
	if op.response(message{Type: "screen_clipboard", RequestID: screenTestID, Count: 1, Data: "eA=="}) != nil {
		t.Fatal("in-flight chunk poisoned stream")
	}
	if len(op.data) != 0 {
		t.Fatal("cancelled text retained")
	}
	if op.response(message{Type: "screen_operation_result", RequestID: screenTestID, Kind: "clipboard_get", Code: "operation_cancelled"}) != nil || op.id != "" {
		t.Fatal("cancel completion")
	}
}

func TestCompletedOperationLateCancellationIsHarmless(t *testing.T) {
	var op screenOperation
	_ = op.request(message{Type: "screen_operation", RequestID: screenTestID, Kind: "clipboard_set"})
	_ = op.request(message{Type: "screen_clipboard", RequestID: screenTestID, Count: 1, Data: "eA=="})
	_ = op.request(message{Type: "screen_operation_cancel", RequestID: screenTestID})
	_ = op.response(message{Type: "screen_clipboard_ack", RequestID: screenTestID})
	if op.response(message{Type: "screen_operation_result", RequestID: screenTestID, Kind: "clipboard_set", Code: "ok"}) != nil {
		t.Fatal("completion raced cancellation")
	}
	if op.request(message{Type: "screen_operation_cancel", RequestID: screenTestID}) != nil {
		t.Fatal("late cancellation poisoned stream")
	}
}

func TestClipboardCreditRacingTimeoutCannotPoisonStream(t *testing.T) {
	var op screenOperation
	_ = op.request(message{Type: "screen_operation", RequestID: screenTestID, Kind: "clipboard_get"})
	_ = op.response(message{Type: "screen_clipboard", RequestID: screenTestID, Count: 1, Data: "eA=="})
	_ = op.response(message{Type: "screen_operation_result", RequestID: screenTestID, Kind: "clipboard_get", Code: "operation_timeout"})
	if op.request(message{Type: "screen_clipboard_ack", RequestID: screenTestID}) != nil {
		t.Fatal("late credited chunk killed Screen")
	}
	if op.request(message{Type: "screen_clipboard_ack", RequestID: screenTestID, Index: 2}) == nil {
		t.Fatal("unrelated credit accepted")
	}
}

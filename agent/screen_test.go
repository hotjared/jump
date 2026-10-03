package main

import (
	"context"
	"encoding/base64"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

const screenTestID = "bd0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"

type fakeDesktop struct {
	frames chan desktopFrame
	inputs chan screenInput
	closed chan struct{}
	once   sync.Once
}

func (f *fakeDesktop) Next(ctx context.Context) (desktopFrame, error) {
	select {
	case v := <-f.frames:
		return v, nil
	case <-ctx.Done():
		return desktopFrame{}, ctx.Err()
	}
}
func (f *fakeDesktop) Input(in screenInput) error { f.inputs <- in; return nil }
func (f *fakeDesktop) Close()                     { f.once.Do(func() { close(f.closed) }) }
func TestScreenLifecycleConcurrencyViewOnlyAndAck(t *testing.T) {
	f := &fakeDesktop{frames: make(chan desktopFrame, 2), inputs: make(chan screenInput, 8), closed: make(chan struct{})}
	sent := make(chan message, 32)
	m := &screenMux{send: func(v message) error { sent <- v; return nil }, launch: func(context.Context) (desktopBridge, error) { return f, nil }}
	defer m.closeAll()
	next := func() message {
		t.Helper()
		select {
		case v := <-sent:
			return v
		case <-time.After(time.Second):
			t.Fatal("missing frame")
			return message{}
		}
	}
	m.handle(message{Type: "screen_open", SessionID: screenTestID})
	if next().Type != "screen_opened" {
		t.Fatal("not opened")
	}
	m.handle(message{Type: "screen_open", SessionID: "ad0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"})
	if next().Code != "screen_busy" {
		t.Fatal("concurrency limit")
	}
	m.handle(message{Type: "screen_mode", SessionID: screenTestID, Mode: "view"})
	if next().Mode != "view" {
		t.Fatal("mode not acknowledged")
	}
	if (<-f.inputs).Action != "release" {
		t.Fatal("held input not released")
	}
	m.handle(message{Type: "screen_input", SessionID: screenTestID, Input: &screenInput{Action: "key", Key: 65, Down: true}})
	m.handle(message{Type: "screen_mode", SessionID: screenTestID, Mode: "control"})
	if next().Mode != "control" {
		t.Fatal("control not acknowledged")
	}
	if (<-f.inputs).Action != "release" {
		t.Fatal("view input reached helper")
	}
	m.handle(message{Type: "screen_input", SessionID: screenTestID, Input: &screenInput{Action: "key", Key: 65, Down: true}})
	select {
	case in := <-f.inputs:
		if in.Key != 65 {
			t.Fatal(in)
		}
	case <-time.After(time.Second):
		t.Fatal("missing input")
	}
	f.frames <- desktopFrame{JPEG: []byte("image"), Width: 10, Height: 10}
	frame := next()
	if frame.Type != "screen_frame" || frame.FrameID != 1 {
		t.Fatal(frame)
	}
	f.frames <- desktopFrame{JPEG: []byte("next"), Width: 10, Height: 10}
	m.handle(message{Type: "screen_ack", SessionID: screenTestID, FrameID: 1})
	if next().FrameID != 2 {
		t.Fatal("frame credit")
	}
	m.handle(message{Type: "screen_close", SessionID: screenTestID})
	select {
	case <-f.closed:
	case <-time.After(time.Second):
		t.Fatal("helper leaked")
	}
	m.handle(message{Type: "screen_input", SessionID: screenTestID, Input: &screenInput{Action: "key", Key: 65, Down: true}})
	select {
	case <-f.inputs:
		t.Fatal("late input")
	default:
	}
}
func TestScreenValidation(t *testing.T) {
	if runtime.GOOS != "windows" && screenSupported() {
		t.Fatal("non-Windows capability")
	}
	for _, in := range []screenInput{{Action: "shell"}, {Action: "key", Key: 0}, {Action: "move", X: -1}, {Action: "wheel", Delta: 1201}, {Action: "button", Button: 3}, {Action: "release", Key: 65}, {Action: "key", Key: 65, Delta: 1}} {
		if validScreenInput(&in) {
			t.Fatal("accepted", in)
		}
	}
	if !validScreenInput(&screenInput{Action: "key", Key: 65, Down: true}) || validScreenID("../../arbitrary-resource") {
		t.Fatal("validation")
	}
	s := screenSequence{}
	m := message{FrameID: 1, Count: 2, Width: 10, Height: 10, Data: base64.StdEncoding.EncodeToString(make([]byte, screenChunkBytes))}
	if s.chunk(m) != nil {
		t.Fatal("first chunk")
	}
	m.Index = 1
	m.Data = base64.StdEncoding.EncodeToString([]byte("last"))
	if s.chunk(m) != nil {
		t.Fatal("last chunk")
	}
	if s.chunk(m) == nil {
		t.Fatal("duplicate chunk")
	}
	m.FrameID = 2
	m.Index = 0
	m.Count = 33
	if s.chunk(m) == nil {
		t.Fatal("oversized frame")
	}
}
func TestScreenLaunchFailureAndInvalidInputCleanup(t *testing.T) {
	sent := make(chan message, 2)
	m := &screenMux{send: func(v message) error { sent <- v; return nil }, launch: func(context.Context) (desktopBridge, error) { return nil, errors.New("no_interactive_session") }}
	m.handle(message{Type: "screen_open", SessionID: screenTestID})
	select {
	case v := <-sent:
		if v.Code != "no_interactive_session" {
			t.Fatal(v)
		}
	case <-time.After(time.Second):
		t.Fatal("no failure")
	}
	m.closeAll()
}
func TestScreenSessionDiscoveryAndPlatformDecision(t *testing.T) {
	for _, id := range []uint32{0, 0xffffffff, 4} {
		foundId, ok := discoverDesktopSession(func() uint32 { return id }, func(uint32) bool { return false })
		if ok || foundId != 0 {
			t.Fatal("no interactive user accepted")
		}
	}
	if id, ok := discoverDesktopSession(func() uint32 { return 4 }, func(id uint32) bool { return id == 4 }); !ok || id != 4 {
		t.Fatal("active user rejected")
	}
	if !screenPlatformSupported("windows", "amd64", 10, true) || screenPlatformSupported("linux", "amd64", 10, true) || screenPlatformSupported("windows", "arm64", 10, true) || screenPlatformSupported("windows", "amd64", 6, true) || screenPlatformSupported("windows", "amd64", 10, false) {
		t.Fatal("platform capability")
	}
}
func TestScreenOversizedCaptureAndInvalidInputCleanUpHelper(t *testing.T) {
	for _, badCapture := range []bool{true, false} {
		f := &fakeDesktop{frames: make(chan desktopFrame, 1), inputs: make(chan screenInput, 2), closed: make(chan struct{})}
		sent := make(chan message, 8)
		m := &screenMux{send: func(v message) error { sent <- v; return nil }, launch: func(context.Context) (desktopBridge, error) { return f, nil }}
		m.handle(message{Type: "screen_open", SessionID: screenTestID})
		select {
		case <-sent:
		case <-time.After(time.Second):
			t.Fatal("not opened")
		}
		if badCapture {
			f.frames <- desktopFrame{JPEG: make([]byte, screenMaxFrame+1), Width: 1920, Height: 1080}
			select {
			case e := <-sent:
				if e.Code != "capture_failed" {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("oversized capture not rejected")
			}
		} else {
			m.handle(message{Type: "screen_input", SessionID: screenTestID, Input: &screenInput{Action: "shell"}})
		}
		select {
		case <-f.closed:
		case <-time.After(time.Second):
			t.Fatal("helper leaked")
		}
		m.closeAll()
	}
}

type actionDesktop struct {
	*fakeDesktop
	events  chan message
	calls   chan string
	text    string
	failure error
}

func (f *actionDesktop) Events() <-chan message { return f.events }
func (f *actionDesktop) Operation(ctx context.Context, kind, text string) (string, error) {
	f.calls <- kind
	if kind == "clipboard_set" {
		f.text = text
	}
	return f.text, f.failure
}
func TestScreenOperationsAndDesktopChangesRetainStream(t *testing.T) {
	f := &actionDesktop{fakeDesktop: &fakeDesktop{frames: make(chan desktopFrame, 1), inputs: make(chan screenInput, 8), closed: make(chan struct{})}, events: make(chan message, 4), calls: make(chan string, 4), failure: errors.New("sas_blocked")}
	sent := make(chan message, 32)
	mux := &screenMux{launch: func(context.Context) (desktopBridge, error) { return f, nil }, send: func(m message) error { sent <- m; return nil }}
	defer mux.closeAll()
	next := func() message {
		select {
		case m := <-sent:
			return m
		case <-time.After(time.Second):
			t.Fatal("missing message")
			return message{}
		}
	}
	mux.handle(message{Type: "screen_open", SessionID: screenTestID, ScreenVersion: 2})
	if next().Type != "screen_opened" {
		t.Fatal("open")
	}
	request := message{Type: "screen_operation", SessionID: screenTestID, RequestID: screenTestID, Kind: "sas"}
	mux.handle(request)
	if next().Code != "sas_blocked" {
		t.Fatal("SAS failure")
	}
	<-f.calls
	for i, name := range []string{"Default", "Winlogon", "Default"} {
		f.events <- message{Type: "screen_event", Stage: "desktop_changed", Desktop: name}
		if next().Stage != "desktop_changed" {
			t.Fatal("transition event")
		}
		f.frames <- desktopFrame{JPEG: []byte("frame"), Width: 10, Height: 10}
		frame := next()
		if frame.FrameID != uint64(i+1) {
			t.Fatal("frame sequence reset")
		}
		mux.handle(message{Type: "screen_ack", SessionID: screenTestID, FrameID: frame.FrameID})
	}
	mux.handle(message{Type: "screen_mode", SessionID: screenTestID, Mode: "view"})
	if next().Mode != "view" {
		t.Fatal("view")
	}
	mux.handle(request)
	if next().Code != "control_required" {
		t.Fatal("SAS allowed in View Only")
	}
	select {
	case <-f.calls:
		t.Fatal("View Only reached OS action")
	default:
	}
	select {
	case <-f.closed:
		t.Fatal("operation or desktop transition killed agent Screen route")
	default:
	}
}

func TestScreenClipboardFullSizeCreditedAgentHelperRoundTrip(t *testing.T) {
	f := &actionDesktop{fakeDesktop: &fakeDesktop{frames: make(chan desktopFrame, 1), inputs: make(chan screenInput, 8), closed: make(chan struct{})}, events: make(chan message, 4), calls: make(chan string, 4)}
	sent := make(chan message, 2)
	mux := &screenMux{launch: func(context.Context) (desktopBridge, error) { return f, nil }, send: func(m message) error { sent <- m; return nil }}
	defer mux.closeAll()
	next := func() message {
		select {
		case m := <-sent:
			return m
		case <-time.After(time.Second):
			t.Fatal("missing operation reply")
			return message{}
		}
	}
	mux.handle(message{Type: "screen_open", SessionID: screenTestID, ScreenVersion: 2})
	next()
	text := make([]byte, screenClipboardMax)
	for i := range text {
		text[i] = 'x'
	}
	mux.handle(message{Type: "screen_operation", SessionID: screenTestID, RequestID: screenTestID, Kind: "clipboard_set"})
	for _, chunk := range clipboardChunks(screenTestID, text) {
		chunk.SessionID = screenTestID
		mux.handle(chunk)
		ack := next()
		if ack.Type != "screen_clipboard_ack" || ack.Index != chunk.Index {
			t.Fatal("upload credit")
		}
	}
	if next().Code != "ok" {
		t.Fatal("set failed")
	}
	mux.handle(message{Type: "screen_operation", SessionID: screenTestID, RequestID: screenTestID, Kind: "clipboard_get"})
	var received []byte
	for index := 0; index < 64; index++ {
		chunk := next()
		if chunk.Type != "screen_clipboard" || chunk.Index != index {
			t.Fatal("download order")
		}
		data, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			t.Fatal(err)
		}
		received = append(received, data...)
		mux.handle(message{Type: "screen_clipboard_ack", SessionID: screenTestID, RequestID: screenTestID, Index: index})
	}
	if next().Code != "ok" || string(received) != string(text) {
		t.Fatal("round trip corrupted")
	}
	select {
	case <-f.closed:
		t.Fatal("clipboard killed Screen helper")
	default:
	}
}

func TestScreenCapabilitiesKeepV1ForLegacyServers(t *testing.T) {
	caps := appendScreenCapabilities([]string{"filesystem"}, true)
	v1, v2 := false, false
	for _, cap := range caps {
		v1 = v1 || cap == "screen_control_v1"
		v2 = v2 || cap == "screen_control_v2"
	}
	if !v1 || !v2 {
		t.Fatal("supported Windows agent must advertise both Screen protocols")
	}
	if got := appendScreenCapabilities([]string{"filesystem"}, false); len(got) != 1 {
		t.Fatal("unsupported agent advertised Screen")
	}
}

func TestLegacyScreenServerReceivesOnlyV1Traffic(t *testing.T) {
	f := &actionDesktop{fakeDesktop: &fakeDesktop{frames: make(chan desktopFrame, 1), inputs: make(chan screenInput, 8), closed: make(chan struct{})}, events: make(chan message, 4), calls: make(chan string, 4)}
	sent := make(chan message, 32)
	mux := &screenMux{launch: func(context.Context) (desktopBridge, error) { return f, nil }, send: func(m message) error { sent <- m; return nil }}
	defer mux.closeAll()
	next := func() message {
		select {
		case m := <-sent:
			return m
		case <-time.After(time.Second):
			t.Fatal("missing v1 message")
			return message{}
		}
	}
	// Old servers check v1 and send the original open without negotiation.
	mux.handle(message{Type: "screen_open", SessionID: screenTestID})
	if next().Type != "screen_opened" {
		t.Fatal("legacy open failed")
	}
	for _, name := range []string{"Default", "Winlogon", "Default"} {
		f.events <- message{Type: "screen_event", Stage: "desktop_changed", Desktop: name}
	}
	mux.handle(message{Type: "screen_operation", SessionID: screenTestID, RequestID: screenTestID, Kind: "sas"})
	mux.handle(message{Type: "screen_mode", SessionID: screenTestID, Mode: "view"})
	if next().Mode != "view" {
		t.Fatal("v2 message leaked to legacy server")
	}
	mux.handle(message{Type: "screen_mode", SessionID: screenTestID, Mode: "control"})
	if next().Mode != "control" {
		t.Fatal("v1 mode switching failed")
	}
	f.frames <- desktopFrame{JPEG: []byte("frame"), Width: 10, Height: 10}
	if next().Type != "screen_frame" {
		t.Fatal("v1 frame failed")
	}
	select {
	case <-f.calls:
		t.Fatal("unnegotiated operation reached helper")
	default:
	}
}

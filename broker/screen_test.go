package main

import (
	"encoding/base64"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestScreenRoutingLateIsolationAndBackpressure(t *testing.T) {
	agent := &websocket.Conn{}
	other := &websocket.Conn{}
	id := "bd0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"
	route := &sessionRoute{protocol: "screen", deviceID: id, agent: agent, frames: make(chan message, 1), closed: make(chan struct{})}
	b := &broker{sessions: map[string]*sessionRoute{id: route}, active: map[string]*websocket.Conn{id: agent}}
	frame := message{Version: 1, Type: "screen_frame", SessionID: id, FrameID: 1, Count: 1, Width: 1, Height: 1, Data: base64.StdEncoding.EncodeToString([]byte("jpeg"))}
	if !b.agentFrame(other, frame) || len(route.frames) != 0 {
		t.Fatal("wrong connection routed")
	}
	if !b.agentFrame(agent, frame) || len(route.frames) != 1 {
		t.Fatal("frame not routed")
	}
	<-route.frames
	frame.FrameID = 2
	b.agentFrame(agent, frame)
	frame.FrameID = 3
	if !b.agentFrame(agent, frame) {
		t.Fatal("presence rejected")
	}
	select {
	case <-route.closed:
	default:
		t.Fatal("overflow did not close")
	}
	if b.active[id] != agent {
		t.Fatal("agent presence lost")
	}
	replacement := &sessionRoute{protocol: "rdp", agent: agent, frames: make(chan message, 1), closed: make(chan struct{})}
	b.sessions[id] = replacement
	if !b.agentFrame(agent, frame) || len(replacement.frames) != 0 {
		t.Fatal("late frame escaped")
	}
}
func TestScreenInvalidFrameClosesOnlyRoute(t *testing.T) {
	id := "bd0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"
	agent := &websocket.Conn{}
	for _, m := range []message{{Type: "screen_frame", Count: 33}, {Type: "screen_frame", Count: 1, Width: 1, Height: 1, FrameID: 1, Data: "invalid base64"}, {Type: "screen_shell"}, {Type: "screen_mode", Mode: "shell"}} {
		r := &sessionRoute{protocol: "screen", agent: agent, frames: make(chan message, 1), closed: make(chan struct{})}
		b := &broker{sessions: map[string]*sessionRoute{id: r}}
		m.SessionID = id
		m.Version = 1
		if !b.agentFrame(agent, m) {
			t.Fatal("invalid screen frame rejected agent")
		}
		select {
		case <-r.closed:
		default:
			t.Fatal("invalid frame accepted")
		}
	}
}
func TestScreenCapabilityConnectionAndConcurrency(t *testing.T) {
	id := "bd0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"
	conn := "ad0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"
	agent := &websocket.Conn{}
	b := &broker{active: map[string]*websocket.Conn{id: agent}, connections: map[string]string{id: conn}, screenCapabilities: map[string]bool{id: true}, sessions: map[string]*sessionRoute{}}
	if !b.screenAllowed(id, conn, "session") || b.screenAllowed(id, "stale", "session") {
		t.Fatal("connection binding")
	}
	b.screenCapabilities[id] = false
	if b.screenAllowed(id, conn, "session") {
		t.Fatal("missing capability")
	}
	b.screenCapabilities[id] = true
	b.sessions["other"] = &sessionRoute{deviceID: id, protocol: "screen"}
	if b.screenAllowed(id, conn, "session") {
		t.Fatal("multiple controllers")
	}
}

func TestPrivateScreenOwnershipExactConnectionAndInputRouting(t *testing.T) {
	t.Run("v1_agent", func(t *testing.T) { testPrivateScreenRouting(t, false, 2) })
	t.Run("v2_agent", func(t *testing.T) { testPrivateScreenRouting(t, true, 2) })
	t.Run("legacy_server_v2_agent", func(t *testing.T) { testPrivateScreenRouting(t, true, 0) })
}

func testPrivateScreenRouting(t *testing.T, supportsV2 bool, screenVersion int) {
	device := "d3977186-1ce5-488a-a9cd-2a6093865771"
	connection := "459b3fa4-1cef-4539-a519-951877b01875"
	owner := "9956b7bd-8f24-4904-8031-8d515282cf2b"
	id := "29bcac87-43b5-48cf-a4a1-ea46c49f2a82"
	b := &broker{token: "test-secret-with-at-least-32-characters", active: make(map[string]*websocket.Conn), connections: map[string]string{device: connection}, screenCapabilities: map[string]bool{device: true}, screenV2Capabilities: map[string]bool{device: supportsV2}, sessions: make(map[string]*sessionRoute)}
	v2 := supportsV2 && screenVersion == 2
	operationError := "unsupported_agent"
	if v2 {
		operationError = "control_required"
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+b.token || r.URL.Query().Get("user_id") != owner || r.URL.Query().Get("device_id") != device {
			http.Error(w, "unauthorized", 403)
		}
	}))
	defer api.Close()
	b.api, b.client = api.URL, api.Client()
	events := make(chan message, 16)
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, e := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer conn.Close()
		b.mu.Lock()
		b.active[device] = conn
		b.mu.Unlock()
		for {
			var msg message
			if conn.ReadJSON(&msg) != nil {
				return
			}
			b.agentFrame(conn, msg)
		}
	}))
	defer agentServer.Close()
	agent, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(agentServer.URL, "http"), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer agent.Close()
	go func() {
		for {
			var m message
			if agent.ReadJSON(&m) != nil {
				return
			}
			events <- m
			switch m.Type {
			case "screen_close":
				_ = agent.WriteJSON(message{Version: 1, Type: "screen_close", SessionID: m.SessionID})
			case "screen_open":
				_ = agent.WriteJSON(message{Version: 1, Type: "screen_opened", SessionID: m.SessionID})
			case "screen_mode":
				if m.Mode == "view" {
					_ = agent.WriteJSON(message{Version: 1, Type: "screen_mode", SessionID: m.SessionID, Mode: m.Mode})
				}
			case "screen_ack":
				_ = agent.WriteJSON(message{Version: 1, Type: "screen_mode", SessionID: m.SessionID, Mode: "control"})
			case "screen_operation":
				if m.Kind == "sas" {
					_ = agent.WriteJSON(message{Version: 1, Type: "screen_operation_result", SessionID: m.SessionID, RequestID: m.RequestID, Kind: m.Kind, Code: "sas_blocked"})
				}
				if m.Kind == "clipboard_get" {
					_ = agent.WriteJSON(message{Version: 1, Type: "screen_clipboard", SessionID: m.SessionID, RequestID: m.RequestID, Count: 1})
				}
			case "screen_clipboard":
				_ = agent.WriteJSON(message{Version: 1, Type: "screen_clipboard_ack", SessionID: m.SessionID, RequestID: m.RequestID, Index: m.Index})
				if m.Index == m.Count-1 {
					_ = agent.WriteJSON(message{Version: 1, Type: "screen_operation_result", SessionID: m.SessionID, RequestID: m.RequestID, Kind: "clipboard_set", Code: "ok"})
				}
			case "screen_clipboard_ack":
				_ = agent.WriteJSON(message{Version: 1, Type: "screen_operation_result", SessionID: m.SessionID, RequestID: m.RequestID, Kind: "clipboard_get", Code: "ok"})
			}
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/screen-streams/{id}", b.internalScreen)
	server := httptest.NewServer(mux)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/screen-streams/" + id + "?device_id=" + device + "&connection_id=" + connection + "&user_id=" + owner
	headers := http.Header{"Authorization": []string{"Bearer " + b.token}}
	if _, response, err := websocket.DefaultDialer.Dial(url, nil); err == nil || response.StatusCode != 401 {
		t.Fatal("public screen route opened")
	}
	if _, response, err := websocket.DefaultDialer.Dial(strings.Replace(url, owner, device, 1), headers); err == nil || response.StatusCode != 403 {
		t.Fatal("other owner accepted")
	}
	if _, response, err := websocket.DefaultDialer.Dial(strings.Replace(url, connection, device, 1), headers); err == nil || response.StatusCode != 409 {
		t.Fatal("stale connection accepted")
	}
	b.mu.Lock()
	b.screenCapabilities[device] = false
	b.mu.Unlock()
	if _, response, err := websocket.DefaultDialer.Dial(url, headers); err == nil || response.StatusCode != 409 {
		t.Fatal("old agent accepted")
	}
	b.mu.Lock()
	b.screenCapabilities[device] = true
	b.mu.Unlock()
	browser, _, e := websocket.DefaultDialer.Dial(url, headers)
	if e != nil {
		t.Fatal(e)
	}
	defer browser.Close()
	browser.SetReadDeadline(time.Now().Add(2 * time.Second))
	browser.WriteJSON(message{Version: 1, Type: "screen_open", SessionID: id, ScreenVersion: screenVersion, Path: "arbitrary-pipe", Secret: "not-forwarded"})
	var reply message
	if browser.ReadJSON(&reply) != nil || reply.Type != "screen_opened" {
		t.Fatal("screen not opened")
	}
	if open := <-events; open.Path != "" || open.Secret != "" || (open.ScreenVersion == 2) != v2 {
		t.Fatal("arbitrary resource forwarded")
	}
	browser.WriteJSON(message{Version: 1, Type: "screen_mode", SessionID: id, Mode: "view"})
	if browser.ReadJSON(&reply) != nil || reply.Mode != "view" {
		t.Fatal("view not acknowledged")
	}
	<-events
	browser.WriteJSON(message{Version: 1, Type: "screen_operation", SessionID: id, RequestID: id, Kind: "sas"})
	if browser.ReadJSON(&reply) != nil || reply.Code != operationError {
		t.Fatal("SAS permitted in View Only")
	}
	browser.WriteJSON(message{Version: 1, Type: "screen_input", SessionID: id, Input: &screenInput{Action: "key", Key: 65, Down: true}})
	browser.WriteJSON(message{Version: 1, Type: "screen_mode", SessionID: id, Mode: "control"})
	browser.WriteJSON(message{Version: 1, Type: "screen_input", SessionID: id, Input: &screenInput{Action: "key", Key: 67, Down: true}})
	browser.WriteJSON(message{Version: 1, Type: "screen_operation", SessionID: id, RequestID: id, Kind: "sas"})
	if browser.ReadJSON(&reply) != nil || reply.Code != operationError {
		t.Fatal("SAS permitted before mode acknowledgement")
	}
	// Ordered barrier: the agent acknowledges only after processing this frame.
	browser.WriteJSON(message{Version: 1, Type: "screen_ack", SessionID: id, FrameID: 99})
	if m := <-events; m.Type != "screen_mode" {
		t.Fatal("view input reached agent")
	}
	if m := <-events; m.Type != "screen_ack" {
		t.Fatal("pending control input reached agent")
	}
	if browser.ReadJSON(&reply) != nil || reply.Mode != "control" {
		t.Fatal("control not acknowledged")
	}
	browser.WriteJSON(message{Version: 1, Type: "screen_input", SessionID: id, Input: &screenInput{Action: "key", Key: 66, Down: true}})
	select {
	case m := <-events:
		if m.Type != "screen_input" || m.Input.Key != 66 {
			t.Fatal("input not routed")
		}
	case <-time.After(time.Second):
		t.Fatal("missing input")
	}
	if !v2 {
		for _, kind := range []string{"sas", "clipboard_set", "clipboard_get"} {
			browser.WriteJSON(message{Version: 1, Type: "screen_operation", SessionID: id, RequestID: id, Kind: kind})
			if browser.ReadJSON(&reply) != nil || reply.Code != "unsupported_agent" || reply.Kind != kind {
				t.Fatal("v1 operation not safely rejected")
			}
		}
		// Upload/cancel traffic can race the unsupported result; none reaches v1.
		browser.WriteJSON(message{Version: 1, Type: "screen_clipboard", SessionID: id, RequestID: id, Count: 1, Data: "eA=="})
		browser.WriteJSON(message{Version: 1, Type: "screen_clipboard_ack", SessionID: id, RequestID: id})
		browser.WriteJSON(message{Version: 1, Type: "screen_operation_cancel", SessionID: id, RequestID: id})
		browser.WriteJSON(message{Version: 1, Type: "screen_input", SessionID: id, Input: &screenInput{Action: "key", Key: 68, Down: true}})
		if m := <-events; m.Type != "screen_input" || m.Input.Key != 68 {
			t.Fatal("v2 traffic reached v1 or basic input stopped")
		}
		browser.WriteJSON(message{Version: 1, Type: "screen_close", SessionID: id})
		if m := <-events; m.Type != "screen_close" {
			t.Fatal("v1 cleanup failed")
		}
		b.mu.Lock()
		online := b.active[device] != nil
		b.mu.Unlock()
		if !online {
			t.Fatal("v1 rejection terminated agent presence")
		}
		return
	}
	// Clipboard messages alone may exceed the 4 KiB input limit.
	browser.WriteJSON(message{Version: 1, Type: "screen_operation", SessionID: id, RequestID: id, Kind: "clipboard_set"})
	if (<-events).Kind != "clipboard_set" {
		t.Fatal("set request")
	}
	text := []byte(strings.Repeat("x", screenChunkBytes) + "世界")
	for _, chunk := range clipboardChunks(id, text) {
		chunk.Version = 1
		chunk.SessionID = id
		browser.WriteJSON(chunk)
		if browser.ReadJSON(&reply) != nil || reply.Type != "screen_clipboard_ack" || reply.Index != chunk.Index {
			t.Fatal("clipboard credit")
		}
		if (<-events).Type != "screen_clipboard" {
			t.Fatal("chunk not routed")
		}
	}
	if browser.ReadJSON(&reply) != nil || reply.Code != "ok" {
		t.Fatal("clipboard completion")
	}
	browser.WriteJSON(message{Version: 1, Type: "screen_operation", SessionID: id, RequestID: id, Kind: "clipboard_get"})
	<-events
	if browser.ReadJSON(&reply) != nil || reply.Type != "screen_clipboard" || reply.Data != "" {
		t.Fatal("empty clipboard")
	}
	browser.WriteJSON(message{Version: 1, Type: "screen_clipboard_ack", SessionID: id, RequestID: id})
	<-events
	if browser.ReadJSON(&reply) != nil || reply.Code != "ok" {
		t.Fatal("get completion")
	}
	browser.WriteJSON(message{Version: 1, Type: "screen_operation", SessionID: id, RequestID: id, Kind: "sas"})
	<-events
	if browser.ReadJSON(&reply) != nil || reply.Code != "sas_blocked" {
		t.Fatal("SAS failure lost")
	}
	browser.WriteJSON(message{Version: 1, Type: "screen_close", SessionID: id})
	select {
	case m := <-events:
		if m.Type != "screen_close" {
			t.Fatal("cleanup frame")
		}
	case <-time.After(time.Second):
		t.Fatal("route leaked")
	}
	b.mu.Lock()
	online := b.active[device] != nil
	b.mu.Unlock()
	if !online {
		t.Fatal("screen closed presence")
	}
}

func TestScreenPendingModesRevokeInput(t *testing.T) {
	r := &sessionRoute{screenMode: "control"}
	if !r.screenControlling() || !r.requestScreenMode("view") || r.screenControlling() {
		t.Fatal("View request must immediately revoke input")
	}
	if r.requestScreenMode("control") {
		t.Fatal("overlapping mode request accepted")
	}
	r.screenMode, r.screenPendingMode = "view", ""
	if !r.requestScreenMode("control") || r.screenControlling() {
		t.Fatal("Control request must wait for acknowledgement")
	}
}

func TestScreenModeAcknowledgementMustMatchPending(t *testing.T) {
	for _, pending := range []string{"", "view", "control"} {
		t.Run("pending_"+pending, func(t *testing.T) {
			id := "bd0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"
			agent := &websocket.Conn{}
			route := &sessionRoute{protocol: "screen", agent: agent, screenMode: "view", screenPendingMode: pending, frames: make(chan message, 1), closed: make(chan struct{})}
			b := &broker{sessions: map[string]*sessionRoute{id: route}}
			b.screenAgentFrame(agent, message{Version: 1, Type: "screen_mode", SessionID: id, Mode: "control"})
			if pending == "control" {
				if !route.screenControlling() {
					t.Fatal("matching acknowledgement did not activate Control")
				}
			} else {
				if route.screenControlling() {
					t.Fatal("unsolicited or mismatched acknowledgement granted Control")
				}
				select {
				case <-route.closed:
				default:
					t.Fatal("invalid acknowledgement did not close route")
				}
			}
		})
	}
}

func TestScreenV2AgentTrafficRequiresNegotiatedRoute(t *testing.T) {
	id := "bd0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"
	agent := &websocket.Conn{}
	route := &sessionRoute{protocol: "screen", agent: agent, frames: make(chan message, 1), closed: make(chan struct{})}
	b := &broker{active: map[string]*websocket.Conn{"device": agent}, sessions: map[string]*sessionRoute{id: route}}
	b.screenAgentFrame(agent, message{Version: 1, Type: "screen_event", SessionID: id, Stage: "desktop_changed", Desktop: "Winlogon"})
	select {
	case <-route.closed:
	default:
		t.Fatal("v2 event accepted on v1 route")
	}
	if b.active["device"] != agent {
		t.Fatal("invalid Screen traffic terminated agent presence")
	}
}

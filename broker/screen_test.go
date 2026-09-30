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
	device := "d3977186-1ce5-488a-a9cd-2a6093865771"
	connection := "459b3fa4-1cef-4539-a519-951877b01875"
	owner := "9956b7bd-8f24-4904-8031-8d515282cf2b"
	id := "29bcac87-43b5-48cf-a4a1-ea46c49f2a82"
	b := &broker{token: "test-secret-with-at-least-32-characters", active: make(map[string]*websocket.Conn), connections: map[string]string{device: connection}, screenCapabilities: map[string]bool{device: true}, sessions: make(map[string]*sessionRoute)}
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
			case "screen_open":
				_ = agent.WriteJSON(message{Version: 1, Type: "screen_opened", SessionID: m.SessionID})
			case "screen_mode":
				_ = agent.WriteJSON(message{Version: 1, Type: "screen_mode", SessionID: m.SessionID, Mode: m.Mode})
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
	browser.WriteJSON(message{Version: 1, Type: "screen_open", SessionID: id, Path: "arbitrary-pipe", Secret: "not-forwarded"})
	var reply message
	if browser.ReadJSON(&reply) != nil || reply.Type != "screen_opened" {
		t.Fatal("screen not opened")
	}
	if open := <-events; open.Path != "" || open.Secret != "" {
		t.Fatal("arbitrary resource forwarded")
	}
	browser.WriteJSON(message{Version: 1, Type: "screen_mode", SessionID: id, Mode: "view"})
	if browser.ReadJSON(&reply) != nil || reply.Mode != "view" {
		t.Fatal("view not acknowledged")
	}
	<-events
	browser.WriteJSON(message{Version: 1, Type: "screen_input", SessionID: id, Input: &screenInput{Action: "key", Key: 65, Down: true}})
	browser.WriteJSON(message{Version: 1, Type: "screen_mode", SessionID: id, Mode: "control"})
	if browser.ReadJSON(&reply) != nil {
		t.Fatal("control not acknowledged")
	}
	if m := <-events; m.Type != "screen_mode" {
		t.Fatal("view input reached agent")
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

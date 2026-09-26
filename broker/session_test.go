package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestInternalSessionRoutesOnlyItsOwnFrames(t *testing.T) {
	deviceID := "d3977186-1ce5-488a-a9cd-2a6093865771"
	one := "29bcac87-43b5-48cf-a4a1-ea46c49f2a82"
	two := "a4bfc02e-d8c4-4c4e-8c79-070e3714ed2f"
	b := &broker{token: "test-token-with-at-least-32-characters", active: make(map[string]*websocket.Conn), sessions: make(map[string]*sessionRoute)}
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil { return }
		b.mu.Lock(); b.active[deviceID] = conn; b.mu.Unlock()
		defer conn.Close()
		for {
			var frame message
			if conn.ReadJSON(&frame) != nil { return }
			if frame.Type == "session_open" {
				_ = b.agentFrame(conn, message{Version: 1, Type: "session_opened", SessionID: frame.SessionID, Fingerprint: "SHA256:test"})
			}
		}
	}))
	defer agentServer.Close()
	agent, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(agentServer.URL, "http"), nil)
	if err != nil { t.Fatal(err) }
	defer agent.Close()
	control := http.NewServeMux()
	control.HandleFunc("GET /internal/sessions/{id}", b.internalSession)
	server := httptest.NewServer(control)
	defer server.Close()
	open := func(id string) *websocket.Conn {
		url := "ws"+strings.TrimPrefix(server.URL, "http")+"/internal/sessions/"+id+"?device_id="+deviceID
		if _, response, err := websocket.DefaultDialer.Dial(url, nil); err == nil || response.StatusCode != 401 { t.Fatal("unauthorized channel opened") }
		headers := http.Header{"Authorization": []string{"Bearer "+b.token}}
		conn, _, err := websocket.DefaultDialer.Dial(url, headers)
		if err != nil { t.Fatal(err) }
		if err := conn.WriteJSON(message{Version: 1, Type: "session_open", SessionID: id, Kind: "linux_password", Username: "root", Secret: "cGFzcw==", Rows: 24, Columns: 80}); err != nil { t.Fatal(err) }
		conn.SetReadDeadline(time.Now().Add(time.Second))
		var reply message
		if err := conn.ReadJSON(&reply); err != nil || reply.SessionID != id || reply.Type != "session_opened" { t.Fatalf("open reply: %v %v", reply, err) }
		return conn
	}
	first, second := open(one), open(two)
	defer first.Close(); defer second.Close()
	b.mu.Lock(); agentConn := b.active[deviceID]; b.mu.Unlock()
	if !b.agentFrame(agentConn, message{Version: 1, Type: "session_data", SessionID: two, Data: "aGk="}) { t.Fatal("valid frame rejected") }
	var frame message
	if err := second.ReadJSON(&frame); err != nil || frame.SessionID != two || frame.Data != "aGk=" { t.Fatalf("wrong destination: %v %v", frame, err) }
	if !b.agentFrame(agentConn, message{Version: 1, Type: "session_data", SessionID: one, Data: "eW8="}) { t.Fatal("valid frame rejected") }
	if err := first.ReadJSON(&frame); err != nil || frame.SessionID != one || frame.Data != "eW8=" { t.Fatalf("wrong destination: %v %v", frame, err) }
	if b.agentFrame(agentConn, message{Version: 1, Type: "session_data", SessionID: one, Data: strings.Repeat("x", 11001)}) { t.Fatal("oversized frame accepted") }
	first.WriteJSON(message{Version: 1, Type: "session_close", SessionID: one})
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock(); _, exists := b.sessions[one]; b.mu.Unlock()
		if !exists { break }
		if time.Now().After(deadline) { t.Fatal("close did not clean route") }
		time.Sleep(time.Millisecond)
	}
}

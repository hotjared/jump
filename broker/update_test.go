package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestAgentUpdateIsPrivateAndConnectionBound(t *testing.T) {
	const deviceID = "11111111-1111-1111-1111-111111111111"
	const connectionID = "22222222-2222-2222-2222-222222222222"
	const operationID = "33333333-3333-3333-3333-333333333333"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	b := &broker{token: strings.Repeat("a", 32), active: map[string]*websocket.Conn{deviceID: conn},
		connections: map[string]string{deviceID: connectionID}, capabilities: map[string]bool{deviceID: true},
		sessions: map[string]*sessionRoute{}}
	request := map[string]string{"operation_id": operationID, "connection_id": connectionID,
		"version": "v0.1.3", "platform": "linux", "architecture": "amd64",
		"download_url": "https://github.com/hotjared/jump/releases/download/v0.1.3/jump-agent-linux-amd64",
		"sha256":       strings.Repeat("a", 64)}
	call := func(id, token string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/internal/devices/"+id+"/agent-update", bytes.NewReader(body))
		r.SetPathValue("id", id)
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		b.agentUpdate(w, r)
		return w
	}
	validToken := "Bearer " + b.token
	if got := call(deviceID, "").Code; got != 401 {
		t.Fatalf("unauthorized: %d", got)
	}
	if got := call("44444444-4444-4444-4444-444444444444", validToken).Code; got != 409 {
		t.Fatalf("wrong device: %d", got)
	}
	b.capabilities[deviceID] = false
	if got := call(deviceID, validToken).Code; got != 409 {
		t.Fatalf("unsupported agent: %d", got)
	}
	b.capabilities[deviceID] = true
	request["connection_id"] = operationID
	if got := call(deviceID, validToken).Code; got != 409 {
		t.Fatalf("stale connection: %d", got)
	}
	request["connection_id"] = connectionID
	b.sessions["session"] = &sessionRoute{deviceID: deviceID}
	if got := call(deviceID, validToken).Code; got != 409 {
		t.Fatalf("active session: %d", got)
	}
	delete(b.sessions, "session")
	fileRoute := &sessionRoute{deviceID: deviceID, agent: conn, closed: make(chan struct{})}
	b.files = map[string]*sessionRoute{"file-transfer": fileRoute}
	if got := call(deviceID, validToken).Code; got != 409 {
		t.Fatalf("active file transfer: %d", got)
	}
	b.closeFileRoute("file-transfer", fileRoute)
	if b.files["file-transfer"] != nil {
		t.Fatal("file route remained after cleanup")
	}
	if got := call(deviceID, validToken).Code; got != 204 {
		t.Fatalf("delivery: %d", got)
	}
	if got := call(deviceID, validToken).Code; got != 409 {
		t.Fatalf("duplicate update: %d", got)
	}
}

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const fileID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const deviceID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
const userID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
const connectionID = "dddddddd-dddd-dddd-dddd-dddddddddddd"

func TestFileFrameIsolationAndBounds(t *testing.T) {
	owner := &websocket.Conn{}
	other := &websocket.Conn{}
	route := &sessionRoute{deviceID: deviceID, agent: owner, frames: make(chan message, 1), closed: make(chan struct{})}
	otherRoute := &sessionRoute{deviceID: connectionID, agent: other, frames: make(chan message, 1), closed: make(chan struct{})}
	b := &broker{files: map[string]*sessionRoute{fileID: route, connectionID: otherRoute}, active: map[string]*websocket.Conn{deviceID: owner, connectionID: other}}
	frame := message{Version: 1, Type: "file_chunk", TransferID: fileID, Data: base64.StdEncoding.EncodeToString([]byte("data"))}
	if !b.agentFrame(other, frame) || len(route.frames) != 0 {
		t.Fatal("cross connection frame delivered")
	}
	if !b.agentFrame(owner, frame) || len(route.frames) != 1 {
		t.Fatal("correct frame lost")
	}
	if !b.agentFrame(owner, frame) {
		t.Fatal("overflow rejected agent")
	}
	if b.files[fileID] != nil {
		t.Fatal("overflow route not closed")
	}
	if b.files[connectionID] != otherRoute || b.active[deviceID] != owner {
		t.Fatal("overflow affected another route or agent presence")
	}
	if b.agentFrame(owner, message{Version: 1, Type: "file_chunk", TransferID: fileID, Data: strings.Repeat("A", 44001)}) {
		t.Fatal("oversized frame accepted")
	}
}
func TestOldAgentCannotOpenFileStream(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer api.Close()
	b := &broker{api: api.URL, token: strings.Repeat("x", 32), client: api.Client(), active: map[string]*websocket.Conn{deviceID: {}}, connections: map[string]string{deviceID: connectionID}, fileCapabilities: map[string]bool{deviceID: false}}
	url := "/internal/file-streams/" + fileID + "?device_id=" + deviceID + "&connection_id=" + connectionID + "&user_id=" + userID
	request := httptest.NewRequest("GET", url, nil)
	request.SetPathValue("id", fileID)
	request.Header.Set("Authorization", "Bearer "+b.token)
	result := httptest.NewRecorder()
	b.internalFile(result, request)
	if result.Code != 409 {
		t.Fatalf("old agent returned %d", result.Code)
	}
}

func TestCancelFileOwnershipAndIdempotence(t *testing.T) {
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
	agent, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	route := &sessionRoute{deviceID: deviceID, ownerID: userID, agent: agent, frames: make(chan message, 1), closed: make(chan struct{})}
	b := &broker{token: strings.Repeat("x", 32), files: map[string]*sessionRoute{fileID: route}}
	call := func(device, user string) int {
		body, _ := json.Marshal(map[string]string{"device_id": device, "user_id": user})
		request := httptest.NewRequest("POST", "/internal/file-streams/"+fileID+"/cancel", bytes.NewReader(body))
		request.SetPathValue("id", fileID)
		request.Header.Set("Authorization", "Bearer "+b.token)
		result := httptest.NewRecorder()
		b.cancelFile(result, request)
		return result.Code
	}
	if got := call(deviceID, connectionID); got != 403 {
		t.Fatalf("wrong owner: %d", got)
	}
	if got := call(connectionID, userID); got != 403 {
		t.Fatalf("wrong device: %d", got)
	}
	if b.files[fileID] != route {
		t.Fatal("unauthorized cancellation closed route")
	}
	if got := call(deviceID, userID); got != 204 {
		t.Fatalf("valid cancel: %d", got)
	}
	select {
	case <-route.closed:
	default:
		t.Fatal("route not closed")
	}
	if b.files[fileID] != nil {
		t.Fatal("route not removed")
	}
	if got := call(deviceID, userID); got != 204 {
		t.Fatalf("idempotent cancel: %d", got)
	}
}

func TestAgentDisconnectClosesOnlyItsFileRoutes(t *testing.T) {
	first, second := &websocket.Conn{}, &websocket.Conn{}
	r1 := &sessionRoute{agent: first, closed: make(chan struct{})}
	r2 := &sessionRoute{agent: second, closed: make(chan struct{})}
	b := &broker{files: map[string]*sessionRoute{fileID: r1, connectionID: r2}}
	b.closeFilesForAgent(first)
	if b.files[fileID] != nil || b.files[connectionID] != r2 {
		t.Fatal("disconnect route isolation")
	}
	select {
	case <-r1.closed:
	default:
		t.Fatal("first route remains open")
	}
	select {
	case <-r2.closed:
		t.Fatal("unrelated route closed")
	default:
	}
}

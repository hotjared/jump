package main

import (
	"encoding/base64"
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
	b := &broker{files: map[string]*sessionRoute{fileID: route}}
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

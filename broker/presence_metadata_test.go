package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHeartbeatForwardsLiveMetadataAndAcceptsOlderAgents(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	heartbeats := make(chan map[string]json.RawMessage, 3)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "identities") {
			json.NewEncoder(w).Encode(map[string]string{"public_key": base64.StdEncoding.EncodeToString(public)})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			var body map[string]json.RawMessage
			json.NewDecoder(r.Body).Decode(&body)
			heartbeats <- body
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer api.Close()
	b := &broker{api: api.URL, token: "test-token-with-at-least-32-characters", client: api.Client(), active: make(map[string]*websocket.Conn)}
	server := httptest.NewServer(http.HandlerFunc(b.ws))
	defer server.Close()
	deviceID := "bd0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?device_id="+deviceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var challenge message
	if err := conn.ReadJSON(&challenge); err != nil {
		t.Fatal(err)
	}
	nonce, _ := base64.StdEncoding.DecodeString(challenge.Challenge)
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(private, append([]byte("jump-agent-v1:"), nonce...)))
	conn.WriteJSON(message{Version: 1, Type: "auth", DeviceID: deviceID, Signature: signature, Metadata: json.RawMessage(`{"hostname":"test","os_family":"linux"}`)})
	var ready message
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatal(err)
	}
	for _, data := range []json.RawMessage{
		nil, json.RawMessage(`{"hostname":"","os_family":"","current_user":""}`),
		json.RawMessage(`{"hostname":"test","os_family":"linux","current_user":"root","interactive_user":"jared"}`),
	} {
		if err := conn.WriteJSON(message{Version: 1, Type: "heartbeat", Metadata: data}); err != nil {
			t.Fatal(err)
		}
		select {
		case body := <-heartbeats:
			if len(body["connection_id"]) == 0 {
				t.Fatal("missing connection guard")
			}
			if strings.Contains(string(data), "jared") {
				if string(body["metadata"]) != string(data) {
					t.Fatalf("live metadata lost: %s", body["metadata"])
				}
			} else if _, exists := body["metadata"]; exists {
				t.Fatal("forwarded an old agent's empty metadata")
			}
		case <-time.After(time.Second):
			t.Fatal("heartbeat did not reach the backend")
		}
	}
}

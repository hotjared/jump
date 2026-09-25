package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestAgentChallengePresenceAndReplay(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	deviceID := "bd0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"
	var mu sync.Mutex
	events := []string{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+"test-token-with-at-least-32-characters" {
			http.Error(w, "unauthorized", 401)
			return
		}
		mu.Lock()
		events = append(events, r.URL.Path)
		mu.Unlock()
		if strings.Contains(r.URL.Path, "identities") {
			json.NewEncoder(w).Encode(map[string]string{"public_key": base64.StdEncoding.EncodeToString(public)})
		} else {
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer api.Close()
	b := &broker{api: api.URL, token: "test-token-with-at-least-32-characters",
		client: api.Client(), active: make(map[string]*websocket.Conn)}
	mux := http.NewServeMux()
	mux.HandleFunc("/connect", b.ws)
	server := httptest.NewServer(mux)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/connect?device_id=" + deviceID
	connect := func() (*websocket.Conn, message) {
		c, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		var challenge message
		if err := c.ReadJSON(&challenge); err != nil {
			t.Fatal(err)
		}
		return c, challenge
	}
	c, challenge := connect()
	nonce, _ := base64.StdEncoding.DecodeString(challenge.Challenge)
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(private, append([]byte("jump-agent-v1:"), nonce...)))
	if err := c.WriteJSON(message{Version: 1, Type: "auth", DeviceID: deviceID, Signature: signature,
		Metadata: json.RawMessage(`{"hostname":"test","os_family":"linux"}`)}); err != nil {
		t.Fatal(err)
	}
	var ready message
	if err := c.ReadJSON(&ready); err != nil || ready.Type != "ready" {
		t.Fatalf("ready: %v %v", ready, err)
	}
	if err := c.WriteJSON(message{Version: 1, Type: "heartbeat"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	c.Close()
	replay, secondChallenge := connect()
	defer replay.Close()
	if secondChallenge.Challenge == challenge.Challenge {
		t.Fatal("nonce reused")
	}
	replay.WriteJSON(message{Version: 1, Type: "auth", DeviceID: deviceID, Signature: signature})
	replay.SetReadDeadline(time.Now().Add(time.Second))
	if err := replay.ReadJSON(&ready); err == nil {
		t.Fatal("replay accepted")
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		snapshot := append([]string(nil), events...)
		mu.Unlock()
		foundConnected, foundHeartbeat, foundDisconnected := false, false, false
		for _, e := range snapshot {
			foundConnected = foundConnected || strings.HasSuffix(e, "/connected")
			foundHeartbeat = foundHeartbeat || strings.HasSuffix(e, "/heartbeat")
			foundDisconnected = foundDisconnected || strings.HasSuffix(e, "/disconnected")
		}
		if foundConnected && foundHeartbeat && foundDisconnected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing presence events: %v", snapshot)
		}
		time.Sleep(10 * time.Millisecond)
	}
	oldInterval, oldTimeout := pingInterval, heartbeatTimeout
	pingInterval, heartbeatTimeout = 15*time.Millisecond, 45*time.Millisecond
	defer func() { pingInterval, heartbeatTimeout = oldInterval, oldTimeout }()
	idle, fresh := connect()
	defer idle.Close()
	freshNonce, _ := base64.StdEncoding.DecodeString(fresh.Challenge)
	freshSignature := base64.StdEncoding.EncodeToString(
		ed25519.Sign(private, append([]byte("jump-agent-v1:"), freshNonce...)),
	)
	if err := idle.WriteJSON(message{
		Version: 1, Type: "auth", DeviceID: deviceID, Signature: freshSignature,
		Metadata: json.RawMessage(`{"hostname":"test","os_family":"linux"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := idle.ReadJSON(&ready); err != nil {
		t.Fatal(err)
	}
	idle.SetReadDeadline(time.Now().Add(time.Second))
	for {
		var incoming message
		if err := idle.ReadJSON(&incoming); err != nil {
			break
		}
	}
	deadline = time.Now().Add(time.Second)
	for {
		mu.Lock()
		snapshot := append([]string(nil), events...)
		mu.Unlock()
		disconnections := 0
		for _, event := range snapshot {
			if strings.HasSuffix(event, "/disconnected") {
				disconnections++
			}
		}
		if disconnections >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle connection was not marked offline: %v", snapshot)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

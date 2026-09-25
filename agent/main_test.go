package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestEnrollmentAndAuthenticatedHeartbeat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JUMP_AGENT_STATE", filepath.Join(dir, "identity.json"))
	const deviceID = "bd0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"
	heartbeat := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/enroll":
			var body struct {
				Token     string `json:"token"`
				PublicKey string `json:"public_key"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Token != "one-use-token" {
				http.Error(w, "invalid", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"device_id": deviceID})
		case "/connect":
			conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			nonce := make([]byte, 32)
			rand.Read(nonce)
			conn.WriteJSON(message{Version: 1, Type: "challenge", Challenge: base64.StdEncoding.EncodeToString(nonce)})
			var auth message
			if conn.ReadJSON(&auth) != nil {
				return
			}
			data, err := os.ReadFile(statePath())
			if err != nil {
				return
			}
			var state identity
			json.Unmarshal(data, &state)
			private, _ := base64.StdEncoding.DecodeString(state.PrivateKey)
			public := ed25519.PrivateKey(private).Public().(ed25519.PublicKey)
			signature, _ := base64.StdEncoding.DecodeString(auth.Signature)
			if !ed25519.Verify(public, append([]byte("jump-agent-v1:"), nonce...), signature) {
				return
			}
			conn.WriteJSON(message{Version: 1, Type: "ready"})
			var beat message
			conn.SetReadDeadline(time.Now().Add(25 * time.Second))
			if conn.ReadJSON(&beat) == nil && beat.Type == "heartbeat" {
				heartbeat <- true
			}
		}
	}))
	defer server.Close()
	if err := enroll(server.URL, "one-use-token"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(statePath())
	if err != nil {
		t.Fatal(err)
	}
	var id identity
	if err := json.Unmarshal(data, &id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- connect(ctx, id) }()
	select {
	case <-heartbeat:
	case <-ctx.Done():
		t.Fatal("heartbeat not received")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not close")
	}
	if err := enroll(server.URL, "one-use-token"); err == nil {
		t.Fatal("overwrote persistent identity")
	}
}

func TestServerRequiresTLS(t *testing.T) {
	if validateServer("http://outside.example") == nil {
		t.Fatal("insecure public server accepted")
	}
	if validateServer("https://agent.example") != nil {
		t.Fatal("HTTPS rejected")
	}
	if validateServer("http://localhost:8080") != nil {
		t.Fatal("local development rejected")
	}
}

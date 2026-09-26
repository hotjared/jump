package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

func localSSHServer(t *testing.T) (string, string, <-chan string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil { t.Fatal(err) }
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil { t.Fatal(err) }
	config := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if meta.User() == "deploy" && string(password) == "correct" { return nil, nil }
		return nil, errors.New("rejected")
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { listener.Close() })
	events := make(chan string, 16)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil { return }
			go func() {
				defer conn.Close()
				_, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil { return }
				go ssh.DiscardRequests(requests)
				for request := range channels {
					if request.ChannelType() != "session" { request.Reject(ssh.UnknownChannelType, "session only"); continue }
					channel, channelRequests, err := request.Accept()
					if err != nil { return }
					go func() {
						defer channel.Close()
						for req := range channelRequests {
							switch req.Type {
							case "pty-req":
								events <- "pty"
								req.Reply(true, nil)
							case "shell":
								events <- "shell"
								req.Reply(true, nil)
								go func() { _, _ = io.Copy(channel, channel) }()
							case "window-change":
								events <- "resize"
							default:
								req.Reply(false, nil)
							}
						}
					}()
				}
			}()
		}
	}()
	return listener.Addr().String(), ssh.FingerprintSHA256(signer.PublicKey()), events
}

func localWebSocket(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err == nil { accepted <- conn; <-done; conn.Close() }
	}))
	t.Cleanup(func() { close(done); server.Close() })
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { client.Close() })
	return <-accepted, client
}

func TestSSHMultiplexedPTYDataResizeAndClose(t *testing.T) {
	address, fingerprint, events := localSSHServer(t)
	serverWS, clientWS := localWebSocket(t)
	m := &sshMux{conn: serverWS, address: address, streams: make(map[string]*sshStream)}
	defer m.closeAll()
	ids := []string{"19a7f33a-8946-4d8a-89b4-85739c413a78", "5892c339-14e0-4d6c-a1a7-4c906312f270"}
	for _, id := range ids {
		m.open(message{Version: 1, Type: "session_open", SessionID: id, Kind: "linux_password", Username: "deploy",
			Secret: base64.StdEncoding.EncodeToString([]byte("correct")), HostKey: fingerprint, Rows: 24, Columns: 80})
	}
	clientWS.SetReadDeadline(time.Now().Add(5*time.Second))
	seen := make(map[string]bool)
	for len(seen) < 2 {
		var frame message
		if err := clientWS.ReadJSON(&frame); err != nil { t.Fatal(err) }
		if frame.Type != "session_opened" || frame.Fingerprint != fingerprint { t.Fatalf("unexpected open: %+v", frame) }
		seen[frame.SessionID] = true
	}
	for i := 0; i < 4; i++ {
		select { case <-events: case <-time.After(time.Second): t.Fatal("missing PTY or shell request") }
	}
	if !m.handle(message{Type: "session_resize", SessionID: ids[0], Rows: 31, Columns: 100}) { t.Fatal("resize rejected") }
	select { case event := <-events: if event != "resize" { t.Fatalf("expected resize, got %s", event) }; case <-time.After(time.Second): t.Fatal("resize not sent") }
	if !m.handle(message{Type: "session_data", SessionID: ids[1], Data: base64.StdEncoding.EncodeToString([]byte("hello"))}) { t.Fatal("input rejected") }
	var output message
	if err := clientWS.ReadJSON(&output); err != nil { t.Fatal(err) }
	if output.Type != "session_data" || output.SessionID != ids[1] || output.Data != base64.StdEncoding.EncodeToString([]byte("hello")) { t.Fatalf("wrong output: %+v", output) }
	m.handle(message{Type: "session_close", SessionID: ids[0]})
	m.mu.Lock(); remaining := len(m.streams); m.mu.Unlock()
	if remaining != 1 { t.Fatalf("closing one stream affected another: %d", remaining) }
	m.handle(message{Type: "session_close", SessionID: ids[1]})
	m.mu.Lock(); remaining = len(m.streams); m.mu.Unlock()
	if remaining != 0 { t.Fatal("streams not cleaned up") }
}

func TestSSHAuthenticationAndHostMismatchErrors(t *testing.T) {
	address, _, _ := localSSHServer(t)
	serverWS, clientWS := localWebSocket(t)
	m := &sshMux{conn: serverWS, address: address, streams: make(map[string]*sshStream)}
	defer m.closeAll()
	inputs := []struct { id, password, hostKey, code string }{
		{"936425c4-f709-4dd2-a7a6-3382842efea8", "wrong", "", "authentication_failed"},
		{"a9b2ef1d-34aa-4bd5-b929-6ff5727c7e26", "correct", "SHA256:wrong", "host_key_mismatch"},
	}
	clientWS.SetReadDeadline(time.Now().Add(5*time.Second))
	for _, input := range inputs {
		m.open(message{Version: 1, Type: "session_open", SessionID: input.id, Kind: "linux_password", Username: "deploy",
			Secret: base64.StdEncoding.EncodeToString([]byte(input.password)), HostKey: input.hostKey, Rows: 24, Columns: 80})
		var frame message
		if err := clientWS.ReadJSON(&frame); err != nil { t.Fatal(err) }
		if frame.Type != "session_error" || frame.SessionID != input.id || frame.Code != input.code { t.Fatalf("unexpected error frame: %+v", frame) }
	}
}

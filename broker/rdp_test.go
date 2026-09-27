package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPrivateRDPStreamBindingIsolationAndUpdateConflict(t *testing.T) {
	device := "d3977186-1ce5-488a-a9cd-2a6093865771"
	connection := "459b3fa4-1cef-4539-a519-951877b01875"
	owner := "9956b7bd-8f24-4904-8031-8d515282cf2b"
	ids := []string{"29bcac87-43b5-48cf-a4a1-ea46c49f2a82", "a4bfc02e-d8c4-4c4e-8c79-070e3714ed2f"}
	b := &broker{token: "test-token-with-at-least-32-characters", active: make(map[string]*websocket.Conn),
		connections: map[string]string{device: connection}, rdpCapabilities: map[string]bool{device: true}, sessions: make(map[string]*sessionRoute)}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+b.token || r.URL.Query().Get("user_id") != owner || r.URL.Query().Get("device_id") != device {
			http.Error(w, "unauthorized", 403)
		}
	}))
	defer api.Close()
	b.api, b.client = api.URL, api.Client()
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b.mu.Lock()
		b.active[device] = conn
		b.mu.Unlock()
		defer conn.Close()
		for {
			var frame message
			if conn.ReadJSON(&frame) != nil {
				return
			}
			b.agentFrame(conn, frame)
		}
	}))
	defer agentServer.Close()
	agent, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(agentServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	go func() {
		for {
			var msg message
			if agent.ReadJSON(&msg) != nil {
				return
			}
			if msg.Type == "tcp_open" || msg.Type == "tcp_data" {
				if msg.Type == "tcp_open" && (msg.Data != "" || msg.Secret != "") {
					t.Error("browser payload reached agent open")
				}
				response := message{Version: 1, Type: "tcp_opened", SessionID: msg.SessionID}
				if msg.Type == "tcp_data" {
					response.Type = "tcp_data"
					response.Data = msg.Data
				}
				_ = agent.WriteJSON(response)
			}
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/rdp-streams/{id}", b.internalTCP)
	server := httptest.NewServer(mux)
	defer server.Close()
	url := func(id, conn string) string {
		return "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/rdp-streams/" + id + "?device_id=" + device + "&connection_id=" + conn + "&user_id=" + owner
	}
	headers := http.Header{"Authorization": []string{"Bearer " + b.token}}
	if _, response, err := websocket.DefaultDialer.Dial(url(ids[0], connection), nil); err == nil || response.StatusCode != 401 {
		t.Fatal("public RDP stream opened")
	}
	if _, response, err := websocket.DefaultDialer.Dial(strings.Replace(url(ids[0], connection), owner, ids[1], 1), headers); err == nil || response.StatusCode != 403 {
		t.Fatal("wrong owner accepted")
	}
	if _, response, err := websocket.DefaultDialer.Dial(url(ids[0], ids[1]), headers); err == nil || response.StatusCode != 409 {
		t.Fatal("wrong agent connection accepted")
	}
	b.mu.Lock()
	b.rdpCapabilities[device] = false
	b.mu.Unlock()
	if _, response, err := websocket.DefaultDialer.Dial(url(ids[0], connection), headers); err == nil || response.StatusCode != 409 {
		t.Fatal("old agent accepted")
	}
	b.mu.Lock()
	b.rdpCapabilities[device] = true
	b.mu.Unlock()
	open := func(id string) *websocket.Conn {
		conn, _, err := websocket.DefaultDialer.Dial(url(id, connection), headers)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.WriteJSON(map[string]any{"version": 1, "type": "tcp_open", "session_id": id, "host": "evil.example", "port": 443, "secret": "never-forward"}); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var reply message
		if err := conn.ReadJSON(&reply); err != nil || reply.Type != "tcp_opened" {
			t.Fatalf("open: %+v %v", reply, err)
		}
		return conn
	}
	first, second := open(ids[0]), open(ids[1])
	defer first.Close()
	defer second.Close()
	for i, stream := range []*websocket.Conn{first, second} {
		stream.WriteJSON(message{Version: 1, Type: "tcp_data", SessionID: ids[i], Data: "aGk="})
		var echoed message
		if err := stream.ReadJSON(&echoed); err != nil || echoed.SessionID != ids[i] || echoed.Data != "aGk=" {
			t.Fatalf("stream isolation: %+v %v", echoed, err)
		}
	}
	b.mu.Lock()
	busy := len(b.sessions)
	b.mu.Unlock()
	if busy != 2 {
		t.Fatal("concurrent streams not routed")
	}
	first.Close()
	second.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		remaining := len(b.sessions)
		b.mu.Unlock()
		if remaining == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream disconnect leaked routes")
}

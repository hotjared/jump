package main

import (
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestScreenRecoveryWaitsForAgentReleaseAndPreservesReservation(t *testing.T) {
	device := "d3977186-1ce5-488a-a9cd-2a6093865771"
	connection := "459b3fa4-1cef-4539-a519-951877b01875"
	owner := "9956b7bd-8f24-4904-8031-8d515282cf2b"
	id := "29bcac87-43b5-48cf-a4a1-ea46c49f2a82"
	b := &broker{token: "secret", active: make(map[string]*websocket.Conn), connections: map[string]string{device: connection}, screenCapabilities: map[string]bool{device: true}, sessions: make(map[string]*sessionRoute)}
	attached := make(chan *websocket.Conn, 1)
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		attached <- conn
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
	socket := <-attached
	route := &sessionRoute{protocol: "screen", deviceID: device, ownerID: owner, agent: socket, closed: make(chan struct{}), screenReleased: make(chan struct{})}
	b.active[device], b.sessions[id] = socket, route
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/screen-streams/{id}/close", b.internalScreenClose)
	server := httptest.NewServer(mux)
	defer server.Close()
	url := server.URL + "/internal/screen-streams/" + id + "/close?device_id=" + device + "&connection_id=" + connection + "&user_id=" + owner
	result := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", url, nil)
		req.Header.Set("Authorization", "Bearer "+b.token)
		response, err := server.Client().Do(req)
		if err != nil {
			result <- 0
			return
		}
		defer response.Body.Close()
		result <- response.StatusCode
	}()
	agent.SetReadDeadline(time.Now().Add(time.Second))
	var closeMessage message
	if agent.ReadJSON(&closeMessage) != nil || closeMessage.Type != "screen_close" || closeMessage.SessionID != id {
		t.Fatal("missing exact-session teardown")
	}
	b.mu.Lock()
	allowed := b.screenAllowed(device, connection, "replacement")
	b.mu.Unlock()
	if allowed {
		t.Fatal("replacement permitted before agent release")
	}
	select {
	case <-route.closed:
	default:
		t.Fatal("gateway not stopped")
	}
	select {
	case <-result:
		t.Fatal("teardown did not wait for acknowledgement")
	default:
	}
	if err := agent.WriteJSON(message{Version: 1, Type: "screen_close", SessionID: id}); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-result:
		if code != 204 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("release not acknowledged")
	}
	b.mu.Lock()
	allowed = b.screenAllowed(device, connection, "replacement")
	b.mu.Unlock()
	if !allowed {
		t.Fatal("released controller still blocks replacement")
	}
	b.releaseScreenRoute(id, route) // Repeated release must not panic.
}

func TestScreenRecoveryRejectsUnrelatedProtocolOwnerAndPublicAccess(t *testing.T) {
	id := "29bcac87-43b5-48cf-a4a1-ea46c49f2a82"
	device := "d3977186-1ce5-488a-a9cd-2a6093865771"
	owner := "9956b7bd-8f24-4904-8031-8d515282cf2b"
	route := &sessionRoute{protocol: "ssh", deviceID: device, ownerID: owner, closed: make(chan struct{})}
	b := &broker{token: "secret", sessions: map[string]*sessionRoute{id: route}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/screen-streams/{id}/close", b.internalScreenClose)
	url := "/internal/screen-streams/" + id + "/close?device_id=" + device + "&user_id=" + owner
	for _, test := range []struct {
		token, origin string
		code          int
	}{{"", "", 401}, {"Bearer secret", "https://public.example", 401}, {"Bearer secret", "", 403}} {
		req := httptest.NewRequest("POST", url, nil)
		req.Header.Set("Authorization", test.token)
		req.Header.Set("Origin", test.origin)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		if response.Code != test.code {
			t.Fatal(response.Code)
		}
	}
	route.protocol, route.ownerID = "screen", device
	req := httptest.NewRequest("POST", url, nil)
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, req)
	if response.Code != 403 {
		t.Fatal("other owner accepted")
	}
	select {
	case <-route.closed:
		t.Fatal("unrelated route closed")
	default:
	}
}

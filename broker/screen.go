package main

import (
	"crypto/subtle"
	"encoding/json"
	"github.com/gorilla/websocket"
	"net/http"
	"time"
)

func (b *broker) screenAgentFrame(agent *websocket.Conn, m message) bool {
	b.mu.Lock()
	r := b.sessions[m.SessionID]
	b.mu.Unlock()
	if r == nil || r.protocol != "screen" || r.agent != agent {
		return true
	}
	valid := m.Version == 1 && validScreenID(m.SessionID)
	r.screenMu.Lock()
	ignored := r.screenOp.cancelled && m.RequestID == r.screenOp.id && m.Type != "screen_operation_result" && (m.Type == "screen_clipboard" || m.Type == "screen_clipboard_ack")
	switch m.Type {
	case "screen_event":
		valid = valid && m.Data == "" && m.Input == nil && safeScreenEvent(m)
	case "screen_clipboard", "screen_clipboard_ack", "screen_operation_result":
		valid = valid && r.screenOp.response(m) == nil
	case "screen_frame":
		valid = valid && r.screenSeq.chunk(m) == nil
	case "screen_opened", "screen_close":
		valid = valid && m.Data == "" && m.Input == nil
	case "screen_error":
		valid = valid && len(m.Code) <= 64 && m.Data == "" && m.Input == nil
	case "screen_mode":
		valid = valid && r.screenPendingMode != "" && m.Mode == r.screenPendingMode && m.Data == "" && m.Input == nil
		if valid {
			r.screenMode = m.Mode
			r.screenPendingMode = ""
		}
	default:
		valid = false
	}
	r.screenMu.Unlock()
	if ignored && valid {
		return true
	}
	if !valid {
		r.markClose("invalid_frame")
		b.closeRoute(m.SessionID, r)
		return true
	}
	select {
	case <-r.closed:
		return true
	default:
	}
	queue := r.frames
	if r.screenActions != nil && (m.Type == "screen_event" || m.Type == "screen_clipboard" || m.Type == "screen_clipboard_ack" || m.Type == "screen_operation_result") {
		queue = r.screenActions
	}
	select {
	case queue <- m:
	default:
		r.markClose("screen_backpressure")
		b.closeRoute(m.SessionID, r)
	}
	return true
}
func (b *broker) screenAllowed(device, connection, id string) bool {
	if b.active[device] == nil || b.connections[device] != connection || !b.screenCapabilities[device] || b.updating[device] || b.sessions[id] != nil {
		return false
	}
	for _, r := range b.sessions {
		if r.deviceID == device && r.protocol == "screen" {
			return false
		}
	}
	return true
}
func (b *broker) internalScreen(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	id, device, connection, user := r.PathValue("id"), r.URL.Query().Get("device_id"), r.URL.Query().Get("connection_id"), r.URL.Query().Get("user_id")
	if !validScreenID(id) || !validScreenID(device) || !validScreenID(connection) || !validScreenID(user) || r.Header.Get("Origin") != "" {
		http.Error(w, "invalid session", 400)
		return
	}
	if b.call(r.Context(), "GET", "/api/internal/screen-streams/"+id+"/authorize?device_id="+device+"&connection_id="+connection+"&user_id="+user, nil, nil) != nil {
		http.Error(w, "session unauthorized", 403)
		return
	}
	b.mu.Lock()
	allowed := b.screenAllowed(device, connection, id)
	agent := b.active[device]
	b.mu.Unlock()
	if !allowed {
		http.Error(w, "agent unavailable", 409)
		return
	}
	conn, e := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if e != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(screenClipboardWireMax)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var open message
	if conn.ReadJSON(&open) != nil || open.Version != 1 || open.Type != "screen_open" || open.SessionID != id || open.Input != nil || open.Data != "" {
		return
	}
	route := &sessionRoute{screenActions: make(chan message, 8), screenMode: "control", deviceID: device, ownerID: user, protocol: "screen", agent: agent, frames: make(chan message, 32), closed: make(chan struct{})}
	b.mu.Lock()
	if !b.screenAllowed(device, connection, id) || b.active[device] != agent {
		b.mu.Unlock()
		return
	}
	if b.sessions == nil {
		b.sessions = make(map[string]*sessionRoute)
	}
	b.sessions[id] = route
	b.mu.Unlock()
	defer func() {
		b.closeRoute(id, route)
		_ = b.write(agent, message{Version: 1, Type: "screen_close", SessionID: id})
	}()
	if b.write(agent, message{Version: 1, Type: "screen_open", SessionID: id}) != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		start := time.Now()
		events := 0
		for {
			var m message
			_, raw, err := conn.ReadMessage()
			if err != nil || len(raw) > screenClipboardWireMax || json.Unmarshal(raw, &m) != nil || m.Type != "screen_clipboard" && len(raw) > 4096 {
				return
			}
			if time.Since(start) > time.Second {
				start = time.Now()
				events = 0
			}
			events++
			if events > 250 {
				return
			}
			if m.Version != 1 || m.SessionID != id || m.Type != "screen_clipboard" && m.Data != "" {
				return
			}
			b.mu.Lock()
			live := b.active[device] == agent && b.connections[device] == connection && b.sessions[id] == route
			b.mu.Unlock()
			if !live {
				return
			}
			switch m.Type {
			case "screen_operation", "screen_clipboard", "screen_clipboard_ack", "screen_operation_cancel":
				route.screenMu.Lock()
				err := route.screenOp.request(m)
				controlling := route.screenMode == "control" && route.screenPendingMode == ""
				if err == nil && !controlling && m.Type != "screen_operation_cancel" {
					reply := message{Version: 1, Type: "screen_operation_result", SessionID: id, RequestID: route.screenOp.id, Kind: route.screenOp.kind, Code: "control_required"}
					route.screenOp.clear()
					route.screenMu.Unlock()
					select {
					case route.frames <- reply:
					case <-route.closed:
						return
					}
					continue
				}
				route.screenMu.Unlock()
				if err != nil {
					return
				}
			case "screen_input":
				if !validScreenInput(m.Input) {
					return
				}
				if !route.screenControlling() {
					continue
				}
			case "screen_mode":
				if !route.requestScreenMode(m.Mode) {
					return
				}
			case "screen_ack":
				if m.FrameID == 0 {
					return
				}
			case "screen_close":
				return
			default:
				return
			}
			if b.write(agent, message{Version: 1, Type: m.Type, SessionID: id, Input: m.Input, Mode: m.Mode, FrameID: m.FrameID, RequestID: m.RequestID, Kind: m.Kind, Data: m.Data, Index: m.Index, Count: m.Count}) != nil {
				return
			}
		}
	}()
	// Preserve the opening handshake before independent action/event traffic.
	select {
	case <-done:
		return
	case <-route.closed:
		return
	case first := <-route.frames:
		conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if conn.WriteJSON(first) != nil || first.Type != "screen_opened" {
			return
		}
	}
	for {
		select {
		case <-done:
			return
		case <-route.closed:
			return
		case m := <-route.screenActions:
			conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if conn.WriteJSON(m) != nil {
				return
			}
		case m := <-route.frames:
			conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if conn.WriteJSON(m) != nil || m.Type == "screen_error" || m.Type == "screen_close" {
				return
			}
		}
	}
}

// A pending transition revokes input immediately, including Control -> View.
// Only the agent acknowledgement can grant Control again.
func (r *sessionRoute) screenControlling() bool {
	r.screenMu.Lock()
	defer r.screenMu.Unlock()
	return r.screenMode == "control" && r.screenPendingMode == ""
}

func (r *sessionRoute) requestScreenMode(mode string) bool {
	r.screenMu.Lock()
	defer r.screenMu.Unlock()
	if (mode != "view" && mode != "control") || r.screenPendingMode != "" {
		return false
	}
	r.screenPendingMode = mode
	return true
}

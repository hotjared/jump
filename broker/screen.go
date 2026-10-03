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
	valid := m.Version == 1 && validScreenID(m.SessionID) && (!screenV2Message(m.Type) || r.screenV2)
	r.screenMu.Lock()
	if r.screenClosing {
		released := valid && m.Type == "screen_close" && m.Data == "" && m.Input == nil
		r.screenMu.Unlock()
		if released {
			b.releaseScreenRoute(m.SessionID, r)
		}
		return true
	}
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
	if conn.ReadJSON(&open) != nil || open.Version != 1 || open.Type != "screen_open" || open.SessionID != id || open.Input != nil || open.Data != "" || open.ScreenVersion != 0 && open.ScreenVersion != 1 && open.ScreenVersion != 2 {
		return
	}
	route := &sessionRoute{screenReleased: make(chan struct{}), screenActions: make(chan message, 8), screenMode: "control", deviceID: device, ownerID: user, protocol: "screen", agent: agent, frames: make(chan message, 32), closed: make(chan struct{})}
	b.mu.Lock()
	if !b.screenAllowed(device, connection, id) || b.active[device] != agent {
		b.mu.Unlock()
		return
	}
	if b.sessions == nil {
		b.sessions = make(map[string]*sessionRoute)
	}
	route.screenV2 = open.ScreenVersion == 2 && b.screenV2Capabilities[device]
	b.sessions[id] = route
	b.mu.Unlock()
	defer func() {
		b.stopScreenRoute(id, route)
	}()
	// Recovery may have been requested after the first authorization check.
	if b.call(r.Context(), "GET", "/api/internal/screen-streams/"+id+"/authorize?device_id="+device+"&connection_id="+connection+"&user_id="+user, nil, nil) != nil {
		return
	}
	screenVersion := 0
	if route.screenV2 {
		screenVersion = 2
	}
	route.screenMu.Lock()
	if route.screenClosing {
		route.screenMu.Unlock()
		return
	}
	err := b.write(agent, message{Version: 1, Type: "screen_open", SessionID: id, ScreenVersion: screenVersion})
	route.screenMu.Unlock()
	if err != nil {
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
				if !route.screenV2 {
					if !validOperationEnvelope(m) {
						return
					}
					if m.Type == "screen_operation" {
						var rejected screenOperation
						if rejected.request(m) != nil {
							return
						}
						reply := message{Version: 1, Type: "screen_operation_result", SessionID: id, RequestID: m.RequestID, Kind: m.Kind, Code: "unsupported_agent"}
						select {
						case route.screenActions <- reply:
						case <-route.closed:
							return
						}
					}
					continue // Includes upload chunks/cancellation already in flight.
				}
				route.screenMu.Lock()
				err := route.screenOp.request(m)
				if err == nil && route.screenOp.id == "" {
					route.screenMu.Unlock()
					continue
				}
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

func (b *broker) releaseScreenRoute(id string, route *sessionRoute) {
	b.closeRoute(id, route)
	route.screenReleaseOnce.Do(func() {
		b.mu.Lock()
		if b.sessions[id] == route {
			delete(b.sessions, id)
		}
		b.mu.Unlock()
		close(route.screenReleased)
	})
}

func (b *broker) stopScreenRoute(id string, route *sessionRoute) {
	route.screenMu.Lock()
	defer route.screenMu.Unlock()
	route.screenClosing = true
	b.closeRoute(id, route) // Wake the gateway, retaining the device reservation.
	_ = b.write(route.agent, message{Version: 1, Type: "screen_close", SessionID: id})
}

// Private, exact-session teardown. A 204 means the agent acknowledged release
// or the original agent connection has gone away. A timeout keeps the route.
func (b *broker) internalScreenClose(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 || r.Header.Get("Origin") != "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, device, connection, owner := r.PathValue("id"), r.URL.Query().Get("device_id"), r.URL.Query().Get("connection_id"), r.URL.Query().Get("user_id")
	if !validScreenID(id) || !validScreenID(device) || !validScreenID(owner) || connection != "" && !validScreenID(connection) {
		http.Error(w, "invalid session", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	route := b.sessions[id]
	if route != nil && (route.protocol != "screen" || route.deviceID != device || route.ownerID != owner) {
		b.mu.Unlock()
		http.Error(w, "session unauthorized", http.StatusForbidden)
		return
	}
	// No stream on this connection can survive its departure.
	if b.active[device] == nil || b.connections[device] != connection {
		b.mu.Unlock()
		if route != nil {
			b.releaseScreenRoute(id, route)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if route == nil {
		// Reserve the ID while closing a never-attached stream. The gateway's
		// second authorization check prevents a late open after release.
		route = &sessionRoute{protocol: "screen", deviceID: device, ownerID: owner, agent: b.active[device], closed: make(chan struct{}), screenReleased: make(chan struct{})}
		if b.sessions == nil {
			b.sessions = make(map[string]*sessionRoute)
		}
		b.sessions[id] = route
	}
	b.mu.Unlock()
	b.stopScreenRoute(id, route)
	select {
	case <-route.screenReleased:
		w.WriteHeader(http.StatusNoContent)
	case <-time.After(6 * time.Second):
		http.Error(w, "controller still closing", http.StatusServiceUnavailable)
	case <-r.Context().Done():
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

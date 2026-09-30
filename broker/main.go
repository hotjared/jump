package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const maxMessage = 64 * 1024

var pingInterval = 25 * time.Second
var heartbeatTimeout = 65 * time.Second

type message struct {
	FrameID       uint64          `json:"frame_id,omitempty"`
	Index         int             `json:"index,omitempty"`
	Count         int             `json:"count,omitempty"`
	Width         int             `json:"width,omitempty"`
	Height        int             `json:"height,omitempty"`
	Mode          string          `json:"mode,omitempty"`
	Input         *screenInput    `json:"input,omitempty"`
	Version       int             `json:"version"`
	Type          string          `json:"type"`
	DeviceID      string          `json:"device_id,omitempty"`
	ConnectionID  string          `json:"connection_id,omitempty"`
	Challenge     string          `json:"challenge,omitempty"`
	Signature     string          `json:"signature,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	SessionID     string          `json:"session_id,omitempty"`
	Kind          string          `json:"kind,omitempty"`
	Username      string          `json:"username,omitempty"`
	Secret        string          `json:"secret,omitempty"`
	HostKey       string          `json:"host_key,omitempty"`
	Fingerprint   string          `json:"fingerprint,omitempty"`
	Code          string          `json:"code,omitempty"`
	Data          string          `json:"data,omitempty"`
	Columns       int             `json:"columns,omitempty"`
	Rows          int             `json:"rows,omitempty"`
	OperationID   string          `json:"operation_id,omitempty"`
	TargetVersion string          `json:"target_version,omitempty"`
	Platform      string          `json:"platform,omitempty"`
	Architecture  string          `json:"architecture,omitempty"`
	DownloadURL   string          `json:"download_url,omitempty"`
	SHA256        string          `json:"sha256,omitempty"`
	State         string          `json:"state,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	TransferID    string          `json:"transfer_id,omitempty"`
	Path          string          `json:"path,omitempty"`
	Name          string          `json:"name,omitempty"`
	Overwrite     bool            `json:"overwrite,omitempty"`
	Size          int64           `json:"size,omitempty"`
	Entries       json.RawMessage `json:"entries,omitempty"`
	Offset        int             `json:"offset,omitempty"`
	More          bool            `json:"more,omitempty"`
}

type sessionRoute struct {
	screenMu          sync.Mutex
	screenSeq         screenSequence
	screenMode        string
	screenPendingMode string
	deviceID          string
	protocol          string
	ownerID           string
	agent             *websocket.Conn
	frames            chan message
	closed            chan struct{}
	once              sync.Once
	firstClose        atomic.Pointer[string]
	toAgentBytes      atomic.Uint64
	toJumpBytes       atomic.Uint64
}

func (r *sessionRoute) markClose(reason string) {
	r.firstClose.CompareAndSwap(nil, &reason)
}

func (r *sessionRoute) closeReason() string {
	if reason := r.firstClose.Load(); reason != nil {
		return *reason
	}
	return "unknown"
}

type broker struct {
	api                string
	token              string
	client             *http.Client
	mu                 sync.Mutex
	active             map[string]*websocket.Conn
	connections        map[string]string
	capabilities       map[string]bool
	screenCapabilities map[string]bool
	rdpCapabilities    map[string]bool
	updates            map[string]*websocket.Conn
	updating           map[string]bool
	writers            sync.Map // *websocket.Conn -> *sync.Mutex
	sessions           map[string]*sessionRoute
	files              map[string]*sessionRoute
	fileCapabilities   map[string]bool
	limits             map[string]window
	wg                 sync.WaitGroup
}

func (b *broker) write(conn *websocket.Conn, value message) error {
	lock, _ := b.writers.LoadOrStore(conn, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteJSON(value)
}

func (b *broker) closeRoute(id string, route *sessionRoute) {
	route.once.Do(func() {
		route.markClose("route_cleanup")
		b.mu.Lock()
		if b.sessions[id] == route {
			delete(b.sessions, id)
		}
		b.mu.Unlock()
		close(route.closed)
	})
}

func (b *broker) agentFrame(conn *websocket.Conn, msg message) bool {
	if strings.HasPrefix(msg.Type, "screen_") {
		return b.screenAgentFrame(conn, msg)
	}
	if strings.HasPrefix(msg.Type, "file_") {
		if len(msg.TransferID) != 36 || len(msg.Data) > 44000 || (msg.Type != "file_list_result" && msg.Type != "file_opened" && msg.Type != "file_chunk" && msg.Type != "file_finished" && msg.Type != "file_error") {
			return false
		}
		if msg.Type == "file_chunk" {
			data, err := base64.StdEncoding.DecodeString(msg.Data)
			if err != nil || len(data) > 32768 {
				return false
			}
		}
		b.mu.Lock()
		route := b.files[msg.TransferID]
		b.mu.Unlock()
		if route == nil || route.agent != conn {
			return true
		}
		select {
		case route.frames <- msg:
			return true
		default:
			b.closeFileRoute(msg.TransferID, route)
			return true
		}
	}
	if msg.Type == "agent_update_status" {
		if len(msg.OperationID) != 36 || (msg.State != "downloading" && msg.State != "installing" && msg.State != "restarting" && msg.State != "failed") {
			return false
		}
		b.mu.Lock()
		owner := b.updates[msg.OperationID]
		var deviceID, connectionID string
		for id, active := range b.active {
			if active == conn {
				deviceID, connectionID = id, b.connections[id]
				break
			}
		}
		b.mu.Unlock()
		if owner != conn || deviceID == "" {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.call(ctx, "POST", "/api/internal/devices/"+deviceID+"/agent-update-status",
			map[string]any{"operation_id": msg.OperationID, "connection_id": connectionID, "state": msg.State, "reason": nullableReason(msg.Reason)}, nil)
		if msg.State == "failed" {
			b.mu.Lock()
			delete(b.updating, deviceID)
			delete(b.updates, msg.OperationID)
			b.mu.Unlock()
		}
		return true
	}
	if len(msg.SessionID) != 36 || len(msg.Data) > 11000 ||
		(msg.Type != "session_opened" && msg.Type != "session_error" && msg.Type != "session_data" && msg.Type != "session_close" &&
			msg.Type != "tcp_opened" && msg.Type != "tcp_error" && msg.Type != "tcp_data" && msg.Type != "tcp_close") {
		if strings.HasPrefix(msg.Type, "tcp_") {
			slog.Warn("rdp agent frame rejected", "data_length", len(msg.Data))
		}
		return false
	}
	b.mu.Lock()
	route := b.sessions[msg.SessionID]
	b.mu.Unlock()
	if route == nil || route.agent != conn || ((strings.HasPrefix(msg.Type, "tcp_")) != (route.protocol == "rdp")) {
		return true // late frames cannot escape their closed session
	}
	if msg.Type == "tcp_data" {
		data, err := base64.StdEncoding.DecodeString(msg.Data)
		if err != nil || len(data) > 8192 {
			route.markClose("agent_frame_rejected")
			slog.Warn("rdp agent frame rejected", "session_id", msg.SessionID, "data_length", len(msg.Data))
			b.closeRoute(msg.SessionID, route)
			return true
		}
	}
	select {
	case route.frames <- msg:
		return true
	default:
		if route.protocol == "rdp" {
			route.markClose("agent_to_broker_queue_overflow")
			slog.Warn("rdp broker queue overflow", "session_id", msg.SessionID, "direction", "agent_to_jump")
		}
		b.closeRoute(msg.SessionID, route)
		return true // close only the slow session; keep presence and other streams alive
	}
}

func (b *broker) closeFileRoute(id string, route *sessionRoute) {
	route.once.Do(func() {
		b.mu.Lock()
		if b.files[id] == route {
			delete(b.files, id)
		}
		b.mu.Unlock()
		close(route.closed)
	})
}

func (b *broker) closeFilesForAgent(conn *websocket.Conn) {
	b.mu.Lock()
	var routes []struct {
		id    string
		route *sessionRoute
	}
	for id, route := range b.files {
		if route.agent == conn {
			routes = append(routes, struct {
				id    string
				route *sessionRoute
			}{id, route})
		}
	}
	b.mu.Unlock()
	for _, item := range routes {
		b.closeFileRoute(item.id, item.route)
	}
}

func (b *broker) cancelFile(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 || r.Header.Get("Origin") != "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	var request struct {
		DeviceID string `json:"device_id"`
		UserID   string `json:"user_id"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 512)).Decode(&request) != nil {
		http.Error(w, "invalid", 400)
		return
	}
	id := r.PathValue("id")
	if len(id) != 36 || len(request.DeviceID) != 36 || len(request.UserID) != 36 {
		http.Error(w, "invalid", 400)
		return
	}
	b.mu.Lock()
	route := b.files[id]
	b.mu.Unlock()
	if route != nil && (route.deviceID != request.DeviceID || route.ownerID != request.UserID) {
		http.Error(w, "forbidden", 403)
		return
	}
	if route != nil {
		b.closeFileRoute(id, route)
		_ = b.write(route.agent, message{Version: 1, Type: "file_cancel", TransferID: id})
	}
	w.WriteHeader(http.StatusNoContent)
}

// One authenticated backend WebSocket owns one transfer. The broker checks the
// database claim and the exact live agent connection before forwarding frames.
func (b *broker) internalFile(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 || r.Header.Get("Origin") != "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	id, deviceID, connectionID, ownerID := r.PathValue("id"), r.URL.Query().Get("device_id"), r.URL.Query().Get("connection_id"), r.URL.Query().Get("user_id")
	if len(id) != 36 || len(deviceID) != 36 || len(connectionID) != 36 || len(ownerID) != 36 {
		http.Error(w, "invalid transfer", 400)
		return
	}
	if b.call(r.Context(), "GET", "/api/internal/file-transfers/"+id+"/authorize?device_id="+deviceID+"&connection_id="+connectionID+"&user_id="+ownerID, nil, nil) != nil {
		http.Error(w, "transfer unauthorized", 403)
		return
	}
	b.mu.Lock()
	agent := b.active[deviceID]
	count := 0
	for _, route := range b.files {
		if route.deviceID == deviceID {
			count++
		}
	}
	allowed := agent != nil && b.connections[deviceID] == connectionID && b.fileCapabilities[deviceID] && b.files[id] == nil && count < 4 && !b.updating[deviceID]
	b.mu.Unlock()
	if !allowed {
		http.Error(w, "agent unavailable", 409)
		return
	}
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxMessage)
	route := &sessionRoute{deviceID: deviceID, ownerID: ownerID, agent: agent, frames: make(chan message, 8), closed: make(chan struct{})}
	b.mu.Lock()
	count = 0
	for _, existing := range b.files {
		if existing.deviceID == deviceID {
			count++
		}
	}
	if b.active[deviceID] != agent || b.connections[deviceID] != connectionID || b.files[id] != nil || !b.fileCapabilities[deviceID] || count >= 4 || b.updating[deviceID] {
		b.mu.Unlock()
		return
	}
	if b.files == nil {
		b.files = make(map[string]*sessionRoute)
	}
	b.files[id] = route
	b.mu.Unlock()
	defer func() {
		b.closeFileRoute(id, route)
		_ = b.write(agent, message{Version: 1, Type: "file_cancel", TransferID: id})
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var msg message
			if conn.ReadJSON(&msg) != nil {
				return
			}
			if msg.Version != 1 || msg.TransferID != id || len(msg.Path) > 4096 || len(msg.Name) > 255 || len(msg.Data) > 44000 || msg.Offset < 0 || msg.Offset > 10000 || (msg.Type != "file_list" && msg.Type != "file_upload_open" && msg.Type != "file_chunk" && msg.Type != "file_finish" && msg.Type != "file_download_open" && msg.Type != "file_cancel" && msg.Type != "file_ack") {
				return
			}
			if msg.Type == "file_chunk" {
				data, err := base64.StdEncoding.DecodeString(msg.Data)
				if err != nil || len(data) > 32768 {
					return
				}
			}
			if b.write(agent, msg) != nil || msg.Type == "file_cancel" {
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		case <-route.closed:
			return
		case msg := <-route.frames:
			select {
			case <-route.closed:
				return
			default:
			}
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if conn.WriteJSON(msg) != nil || msg.Type == "file_finished" || msg.Type == "file_error" || msg.Type == "file_list_result" {
				return
			}
		}
	}
}

func nullableReason(reason string) any {
	if reason == "" {
		return nil
	}
	return reason
}

// The private listener accepts only backend-generated release metadata and
// delivers to the exact authenticated connection the backend authorized.
func (b *broker) agentUpdate(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "origin forbidden", 403)
		return
	}
	var req struct {
		OperationID  string `json:"operation_id"`
		ConnectionID string `json:"connection_id"`
		Version      string `json:"version"`
		Platform     string `json:"platform"`
		Architecture string `json:"architecture"`
		DownloadURL  string `json:"download_url"`
		SHA256       string `json:"sha256"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || len(req.OperationID) != 36 || len(req.ConnectionID) != 36 || len(req.SHA256) != 64 ||
		(req.Platform != "linux" && req.Platform != "windows") || req.Architecture != "amd64" || !validUpdateURL(req.Version, req.Platform, req.DownloadURL) {
		http.Error(w, "invalid update", 400)
		return
	}
	id := r.PathValue("id")
	b.mu.Lock()
	conn := b.active[id]
	busy := false
	for _, route := range b.sessions {
		if route.deviceID == id {
			busy = true
			break
		}
	}
	for _, route := range b.files {
		if route.deviceID == id {
			busy = true
			break
		}
	}
	if conn == nil || b.connections[id] != req.ConnectionID || !b.capabilities[id] || busy || b.updating[id] {
		b.mu.Unlock()
		http.Error(w, "agent unavailable or sessions active", 409)
		return
	}
	if b.updates == nil {
		b.updates = make(map[string]*websocket.Conn)
	}
	if b.updating == nil {
		b.updating = make(map[string]bool)
	}
	b.updating[id] = true
	b.updates[req.OperationID] = conn
	b.mu.Unlock()
	err := b.write(conn, message{Version: 1, Type: "agent_update", OperationID: req.OperationID,
		TargetVersion: req.Version, Platform: req.Platform, Architecture: req.Architecture,
		DownloadURL: req.DownloadURL, SHA256: req.SHA256})
	if err != nil {
		b.mu.Lock()
		delete(b.updates, req.OperationID)
		delete(b.updating, id)
		b.mu.Unlock()
		http.Error(w, "agent unavailable", 409)
		return
	}
	w.WriteHeader(204)
}

func validUpdateURL(version, platform, raw string) bool {
	if len(version) < 2 || version[0] != 'v' {
		return false
	}
	for _, part := range strings.Split(version[1:], ".") {
		if part == "" {
			return false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	if len(strings.Split(version[1:], ".")) != 3 {
		return false
	}
	asset := "jump-agent-linux-amd64"
	if platform == "windows" {
		asset = "jump-agent-windows-amd64.exe"
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.RawQuery == "" && u.Fragment == "" &&
		u.Path == "/hotjared/jump/releases/download/"+version+"/"+asset
}

// Only the backend on the private Compose network can open this authenticated
// internal channel. One channel represents one logical SSH stream; the agent
// still uses its single persistent, authenticated WebSocket.
func (b *broker) internalSession(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	id, deviceID := r.PathValue("id"), r.URL.Query().Get("device_id")
	if len(id) != 36 || len(deviceID) != 36 || r.Header.Get("Origin") != "" {
		http.Error(w, "invalid session", 400)
		return
	}
	b.mu.Lock()
	agent := b.active[deviceID]
	_, exists := b.sessions[id]
	b.mu.Unlock()
	if agent == nil || exists {
		http.Error(w, "agent unavailable", 409)
		return
	}
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxMessage)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var open message
	if conn.ReadJSON(&open) != nil || open.Version != 1 || open.Type != "session_open" || open.SessionID != id ||
		(open.Kind != "linux_password" && open.Kind != "linux_ssh_key") || len(open.Secret) > 24000 ||
		len(open.Username) > 255 || open.Columns < 20 || open.Columns > 500 || open.Rows < 5 || open.Rows > 200 {
		return
	}
	route := &sessionRoute{deviceID: deviceID, protocol: "ssh", agent: agent, frames: make(chan message, 16), closed: make(chan struct{})}
	b.mu.Lock()
	if b.active[deviceID] != agent || b.sessions[id] != nil || b.updating[deviceID] {
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
		_ = b.write(agent, message{Version: 1, Type: "session_close", SessionID: id})
	}()
	if b.write(agent, open) != nil {
		return
	}
	open.Secret = ""
	conn.SetReadDeadline(time.Time{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			var msg message
			if conn.ReadJSON(&msg) != nil {
				return
			}
			if msg.Version != 1 || msg.SessionID != id || len(msg.Data) > 11000 ||
				(msg.Type != "session_data" && msg.Type != "session_resize" && msg.Type != "session_close") ||
				(msg.Type == "session_resize" && (msg.Columns < 20 || msg.Columns > 500 || msg.Rows < 5 || msg.Rows > 200)) {
				return
			}
			if b.write(agent, msg) != nil || msg.Type == "session_close" {
				return
			}
		}
	}()
	for {
		select {
		case <-readDone:
			return
		case <-route.closed:
			return
		case msg := <-route.frames:
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if conn.WriteJSON(msg) != nil || msg.Type == "session_close" || msg.Type == "session_error" {
				return
			}
		}
	}
}

// This private, bearer-authenticated channel routes one fixed-purpose RDP
// stream. It never accepts a destination address or port.
func (b *broker) internalTCP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	id, deviceID, connectionID, ownerID := r.PathValue("id"), r.URL.Query().Get("device_id"), r.URL.Query().Get("connection_id"), r.URL.Query().Get("user_id")
	if len(id) != 36 || len(deviceID) != 36 || len(connectionID) != 36 || len(ownerID) != 36 || r.Header.Get("Origin") != "" {
		http.Error(w, "invalid stream", 400)
		return
	}
	if b.call(r.Context(), "GET", "/api/internal/rdp-streams/"+id+"/authorize?device_id="+deviceID+"&connection_id="+connectionID+"&user_id="+ownerID, nil, nil) != nil {
		http.Error(w, "session unauthorized", 403)
		return
	}
	b.mu.Lock()
	agent := b.active[deviceID]
	allowed := agent != nil && b.connections[deviceID] == connectionID && b.rdpCapabilities[deviceID] && !b.updating[deviceID] && b.sessions[id] == nil
	b.mu.Unlock()
	if !allowed {
		http.Error(w, "agent unavailable", 409)
		return
	}
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxMessage)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var open message
	if conn.ReadJSON(&open) != nil || open.Version != 1 || open.Type != "tcp_open" || open.SessionID != id {
		return
	}
	route := &sessionRoute{deviceID: deviceID, protocol: "rdp", ownerID: ownerID, agent: agent, frames: make(chan message, 16), closed: make(chan struct{})}
	b.mu.Lock()
	if b.active[deviceID] != agent || b.connections[deviceID] != connectionID || !b.rdpCapabilities[deviceID] || b.sessions[id] != nil || b.updating[deviceID] {
		b.mu.Unlock()
		return
	}
	b.sessions[id] = route
	b.mu.Unlock()
	defer func() {
		b.closeRoute(id, route)
		_ = b.write(agent, message{Version: 1, Type: "tcp_close", SessionID: id})
		slog.Info("rdp broker stream closed", "session_id", id, "first_close", route.closeReason(),
			"jump_to_agent_bytes", route.toAgentBytes.Load(), "agent_to_jump_bytes", route.toJumpBytes.Load())
	}()
	slog.Info("rdp broker stream opened", "session_id", id, "device_id", deviceID)
	if b.write(agent, message{Version: 1, Type: "tcp_open", SessionID: id}) != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			var msg message
			if conn.ReadJSON(&msg) != nil {
				route.markClose("jump_websocket_closed")
				return
			}
			if msg.Version != 1 || msg.SessionID != id || len(msg.Data) > 11000 || (msg.Type != "tcp_data" && msg.Type != "tcp_close") {
				route.markClose("jump_frame_rejected")
				slog.Warn("rdp jump frame rejected", "session_id", id, "data_length", len(msg.Data))
				return
			}
			var payloadBytes int
			if msg.Type == "tcp_data" {
				data, err := base64.StdEncoding.DecodeString(msg.Data)
				if err != nil || len(data) > 8192 {
					route.markClose("jump_frame_rejected")
					slog.Warn("rdp jump frame rejected", "session_id", id, "data_length", len(msg.Data))
					return
				}
				payloadBytes = len(data)
			}
			if b.write(agent, msg) != nil {
				route.markClose("agent_websocket_write_failed")
				return
			}
			if msg.Type == "tcp_close" {
				route.markClose("jump_tcp_close")
				return
			}
			route.toAgentBytes.Add(uint64(payloadBytes))
		}
	}()
	for {
		select {
		case <-readDone:
			return
		case <-route.closed:
			return
		case msg := <-route.frames:
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if conn.WriteJSON(msg) != nil {
				route.markClose("jump_websocket_write_failed")
				return
			}
			if msg.Type == "tcp_opened" {
				slog.Info("rdp agent TCP opened", "session_id", id)
			}
			if msg.Type == "tcp_data" {
				if data, err := base64.StdEncoding.DecodeString(msg.Data); err == nil {
					route.toJumpBytes.Add(uint64(len(data)))
				}
			}
			if msg.Type == "tcp_close" || msg.Type == "tcp_error" {
				route.markClose("agent_tcp_close")
				return
			}
		}
	}
}

type window struct {
	since time.Time
	count int
}

func (b *broker) allow(r *http.Request, category string, max int) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	key := category + ":" + host
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limits == nil {
		b.limits = make(map[string]window)
	}
	current := b.limits[key]
	if time.Since(current.since) > time.Minute {
		current = window{since: time.Now()}
	}
	current.count++
	b.limits[key] = current
	// Bound memory from stale unauthenticated clients.
	if len(b.limits) > 4096 {
		for k, v := range b.limits {
			if time.Since(v.since) > time.Minute {
				delete(b.limits, k)
			}
		}
	}
	return current.count <= max
}

func (b *broker) call(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.api+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("api returned %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, maxMessage)).Decode(out)
	}
	return nil
}

func (b *broker) enroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", 405)
		return
	}
	if !b.allow(r, "enroll", 10) {
		http.Error(w, "too many attempts", 429)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessage)
	var request struct {
		Token     string          `json:"token"`
		PublicKey string          `json:"public_key"`
		Metadata  json.RawMessage `json:"metadata"`
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Token) > 128 || len(request.Metadata) == 0 {
		http.Error(w, "invalid enrollment", 400)
		return
	}
	var response json.RawMessage
	if err := b.call(r.Context(), "POST", "/api/internal/enroll", request, &response); err != nil {
		slog.Warn("enrollment rejected", "reason", err.Error())
		http.Error(w, "enrollment rejected", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

func (b *broker) ws(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", 405)
		return
	}
	if !b.allow(r, "connect", 60) {
		http.Error(w, "too many attempts", 429)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "origin forbidden", 403)
		return
	}
	id := r.URL.Query().Get("device_id")
	if len(id) != 36 {
		http.Error(w, "invalid device id", 400)
		return
	}
	var identity struct {
		PublicKey string `json:"public_key"`
	}
	if err := b.call(r.Context(), "GET", "/api/internal/identities/"+id, nil, &identity); err != nil {
		http.Error(w, "unknown device", 401)
		return
	}
	pub, err := base64.StdEncoding.DecodeString(identity.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		http.Error(w, "invalid identity", 500)
		return
	}
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxMessage)
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if conn.WriteJSON(message{Version: 1, Type: "challenge", Challenge: base64.StdEncoding.EncodeToString(nonce)}) != nil {
		return
	}
	var auth message
	if conn.ReadJSON(&auth) != nil || auth.Version != 1 || auth.Type != "auth" || auth.DeviceID != id {
		return
	}
	signature, err := base64.StdEncoding.DecodeString(auth.Signature)
	if err != nil || !ed25519.Verify(pub, append([]byte("jump-agent-v1:"), nonce...), signature) {
		return
	}
	connectionID := randomID()
	var authInfo struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(auth.Metadata, &authInfo) != nil {
		return
	}
	updateCapable, rdpCapable, fileCapable, screenCapable := false, false, false, false
	for _, capability := range authInfo.Capabilities {
		if capability == "screen_control_v1" {
			screenCapable = true
		}
		if capability == "agent_update_v1" {
			updateCapable = true
		}
		if capability == "rdp_tunnel_v1" {
			rdpCapable = true
		}
		if capability == "file_transfer_v1" {
			fileCapable = true
		}
	}
	// Serialize registration with a revocation disconnect. The API locks the
	// device row and rejects revoked identities even if the key was fetched
	// before the admin revoked it.
	b.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = b.call(ctx, "POST", "/api/internal/devices/"+id+"/connected",
		map[string]any{"connection_id": connectionID, "metadata": auth.Metadata}, nil)
	cancel()
	if err != nil {
		b.mu.Unlock()
		return
	}
	previous := b.active[id]
	b.active[id] = conn
	if b.connections == nil {
		b.connections = make(map[string]string)
	}
	if b.capabilities == nil {
		b.capabilities = make(map[string]bool)
	}
	if b.rdpCapabilities == nil {
		b.rdpCapabilities = make(map[string]bool)
	}
	if b.fileCapabilities == nil {
		b.fileCapabilities = make(map[string]bool)
	}
	if b.screenCapabilities == nil {
		b.screenCapabilities = make(map[string]bool)
	}
	b.screenCapabilities[id] = screenCapable
	b.connections[id] = connectionID
	b.capabilities[id] = updateCapable
	b.rdpCapabilities[id] = rdpCapable
	b.fileCapabilities[id] = fileCapable
	delete(b.updating, id)
	b.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	b.wg.Add(1)
	defer b.wg.Done()
	defer func() {
		b.mu.Lock()
		if b.active[id] == conn {
			delete(b.active, id)
			delete(b.connections, id)
			delete(b.capabilities, id)
			delete(b.rdpCapabilities, id)
			delete(b.screenCapabilities, id)
			delete(b.fileCapabilities, id)
			delete(b.updating, id)
		}
		for operation, owner := range b.updates {
			if owner == conn {
				delete(b.updates, operation)
			}
		}
		var routes []*sessionRoute
		for _, route := range b.sessions {
			if route.agent == conn {
				routes = append(routes, route)
			}
		}
		b.mu.Unlock()
		for _, route := range routes {
			b.closeRouteForAgent(route)
		}
		b.closeFilesForAgent(conn)
		b.writers.Delete(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := b.call(ctx, "POST", "/api/internal/devices/"+id+"/disconnected",
			map[string]string{"connection_id": connectionID}, nil); err != nil {
			slog.Warn("disconnect update failed", "device_id", id, "error", err)
		}
	}()
	if b.write(conn, message{Version: 1, Type: "ready", ConnectionID: connectionID}) != nil {
		return
	}
	conn.SetReadDeadline(time.Now().Add(75 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(75 * time.Second)) })
	var lastHeartbeat atomic.Int64
	lastHeartbeat.Store(time.Now().UnixNano())
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var msg message
			if conn.ReadJSON(&msg) != nil {
				return
			}
			if msg.Version != 1 {
				return
			}
			if msg.Type != "heartbeat" {
				if !b.agentFrame(conn, msg) {
					return
				}
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := b.call(ctx, "POST", "/api/internal/devices/"+id+"/heartbeat",
				map[string]string{"connection_id": connectionID}, nil)
			cancel()
			if err != nil {
				return
			}
			lastHeartbeat.Store(time.Now().UnixNano())
			conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		}
	}()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, lastHeartbeat.Load())) > heartbeatTimeout {
				slog.Warn("agent heartbeat timed out", "device_id", id)
				return
			}
			lock, _ := b.writers.LoadOrStore(conn, &sync.Mutex{})
			mu := lock.(*sync.Mutex)
			mu.Lock()
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := conn.WriteMessage(websocket.PingMessage, nil)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (b *broker) closeRouteForAgent(route *sessionRoute) {
	b.mu.Lock()
	for id, candidate := range b.sessions {
		if candidate == route {
			b.mu.Unlock()
			b.closeRoute(id, route)
			return
		}
	}
	b.mu.Unlock()
}

// The control listener is private to the Compose network and additionally
// authenticated with the broker-to-API bearer secret.
func (b *broker) disconnect(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare(
		[]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token),
	) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if len(id) != 36 {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	if conn := b.active[id]; conn != nil {
		conn.Close()
	}
	b.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// diagnostics exposes only aggregate sizes from the private control listener.
func (b *broker) diagnostics(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 || r.Header.Get("Origin") != "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	b.mu.Lock()
	counts := struct {
		AgentConnections  int `json:"agent_connections"`
		SessionRoutes     int `json:"session_routes"`
		FileRoutes        int `json:"file_routes"`
		UpdatesInProgress int `json:"updates_in_progress"`
	}{len(b.active), len(b.sessions), len(b.files), len(b.updating)}
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(counts)
}

func randomID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	token := os.Getenv("BROKER_INTERNAL_TOKEN")
	if len(token) < 32 {
		slog.Error("BROKER_INTERNAL_TOKEN is missing")
		os.Exit(1)
	}
	b := &broker{api: strings.TrimRight(os.Getenv("JUMP_INTERNAL_URL"), "/"), token: token,
		client: &http.Client{Timeout: 10 * time.Second}, active: make(map[string]*websocket.Conn), sessions: make(map[string]*sessionRoute)}
	if b.api == "" {
		slog.Error("JUMP_INTERNAL_URL is missing")
		os.Exit(1)
	}
	// A simultaneous Compose restart can start the broker before the API is accepting
	// connections. Reconcile before accepting agents, with a bounded startup wait.
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 60*time.Second)
	var err error
	for {
		attemptCtx, attemptCancel := context.WithTimeout(startupCtx, 5*time.Second)
		err = b.call(attemptCtx, "POST", "/api/internal/reconcile", nil, nil)
		attemptCancel()
		if err == nil {
			break
		}
		select {
		case <-startupCtx.Done():
			slog.Error("startup reconciliation failed", "error", err)
			os.Exit(1)
		case <-time.After(500 * time.Millisecond):
		}
	}
	startupCancel()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/enroll", b.enroll)
	mux.HandleFunc("/connect", b.ws)
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("GET /internal/diagnostics", b.diagnostics)
	internalMux.HandleFunc("POST /internal/devices/{id}/disconnect", b.disconnect)
	internalMux.HandleFunc("POST /internal/devices/{id}/agent-update", b.agentUpdate)
	internalMux.HandleFunc("GET /internal/sessions/{id}", b.internalSession)
	internalMux.HandleFunc("GET /internal/rdp-streams/{id}", b.internalTCP)
	internalMux.HandleFunc("GET /internal/screen-streams/{id}", b.internalScreen)
	internalMux.HandleFunc("GET /internal/file-streams/{id}", b.internalFile)
	internalMux.HandleFunc("POST /internal/file-streams/{id}/cancel", b.cancelFile)
	internalServer := &http.Server{Addr: ":8081", Handler: internalMux, ReadHeaderTimeout: 5 * time.Second}
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	shutdown, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server stopped", "error", err)
			stop()
		}
	}()
	go func() {
		if err := internalServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("internal server stopped", "error", err)
			stop()
		}
	}()
	slog.Info("broker ready")
	<-shutdown.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	internalServer.Shutdown(ctx)
	b.mu.Lock()
	for _, conn := range b.active {
		conn.Close()
	}
	b.mu.Unlock()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("timed out waiting for agent disconnects")
	}
}

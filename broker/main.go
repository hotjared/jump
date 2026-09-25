package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const maxMessage = 16 * 1024

var pingInterval = 25 * time.Second
var heartbeatTimeout = 65 * time.Second

type message struct {
	Version      int             `json:"version"`
	Type         string          `json:"type"`
	DeviceID     string          `json:"device_id,omitempty"`
	ConnectionID string          `json:"connection_id,omitempty"`
	Challenge    string          `json:"challenge,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
}

type broker struct {
	api    string
	token  string
	client *http.Client
	mu     sync.Mutex
	active map[string]*websocket.Conn
	limits map[string]window
	wg     sync.WaitGroup
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = b.call(ctx, "POST", "/api/internal/devices/"+id+"/connected",
		map[string]any{"connection_id": connectionID, "metadata": auth.Metadata}, nil)
	cancel()
	if err != nil {
		return
	}
	b.mu.Lock()
	previous := b.active[id]
	b.active[id] = conn
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
		}
		b.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := b.call(ctx, "POST", "/api/internal/devices/"+id+"/disconnected",
			map[string]string{"connection_id": connectionID}, nil); err != nil {
			slog.Warn("disconnect update failed", "device_id", id, "error", err)
		}
	}()
	if conn.WriteJSON(message{Version: 1, Type: "ready", ConnectionID: connectionID}) != nil {
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
			if msg.Version != 1 || msg.Type != "heartbeat" {
				return
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
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if conn.WriteMessage(websocket.PingMessage, nil) != nil {
				return
			}
		}
	}
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
		client: &http.Client{Timeout: 10 * time.Second}, active: make(map[string]*websocket.Conn)}
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
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	shutdown, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server stopped", "error", err)
			stop()
		}
	}()
	slog.Info("broker ready")
	<-shutdown.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Shutdown(ctx)
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

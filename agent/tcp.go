package main

import (
	"context"
	"encoding/base64"
	"net"
	"runtime"
	"sync"
	"time"
)

// The address is never received from the broker. Tests may substitute a local
// listener; production always dials the Windows loopback RDP service.
const rdpAddress = "127.0.0.1:3389"

type tcpStream struct {
	conn  net.Conn
	queue chan []byte
	done  chan struct{}
	once  sync.Once
}

type tcpMux struct {
	send   func(message) error
	ctx    context.Context
	cancel context.CancelFunc
	// Test-only local listener override; production always uses rdpAddress.
	address     string
	testWindows bool
	mu          sync.Mutex
	streams     map[string]*tcpStream
}

func (m *tcpMux) close(id string, s *tcpStream) {
	s.once.Do(func() {
		close(s.done)
		_ = s.conn.Close()
		m.mu.Lock()
		if m.streams[id] == s {
			delete(m.streams, id)
		}
		m.mu.Unlock()
	})
}

func (m *tcpMux) closeAll() {
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Lock()
	copy := make(map[string]*tcpStream, len(m.streams))
	for id, s := range m.streams {
		copy[id] = s
	}
	m.mu.Unlock()
	for id, s := range copy {
		m.close(id, s)
	}
}

func (m *tcpMux) handle(msg message) bool {
	if msg.Type != "tcp_open" && msg.Type != "tcp_data" && msg.Type != "tcp_close" {
		return false
	}
	if len(msg.SessionID) != 36 {
		return true
	}
	if runtime.GOOS != "windows" && !m.testWindows {
		if msg.Type == "tcp_open" {
			_ = m.send(message{Version: 1, Type: "tcp_error", SessionID: msg.SessionID, Code: "unsupported_agent"})
		}
		return true
	}
	if msg.Type == "tcp_open" {
		m.mu.Lock()
		busy := len(m.streams) >= 16 || m.streams[msg.SessionID] != nil
		m.mu.Unlock()
		if busy {
			_ = m.send(message{Version: 1, Type: "tcp_error", SessionID: msg.SessionID, Code: "agent_unavailable"})
			return true
		}
		go m.open(msg.SessionID)
		return true
	}
	m.mu.Lock()
	s := m.streams[msg.SessionID]
	m.mu.Unlock()
	if s == nil {
		return true
	}
	if msg.Type == "tcp_close" {
		m.close(msg.SessionID, s)
		return true
	}
	if len(msg.Data) > 11000 {
		m.close(msg.SessionID, s)
		return true
	}
	data, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil || len(data) > 8192 {
		m.close(msg.SessionID, s)
		return true
	}
	select {
	case s.queue <- data:
	case <-s.done:
	default:
		m.close(msg.SessionID, s)
	}
	return true
}

func (m *tcpMux) open(id string) {
	parent := m.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	address := rdpAddress
	if m.address != "" {
		address = m.address
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		if parent.Err() == nil {
			_ = m.send(message{Version: 1, Type: "tcp_error", SessionID: id, Code: "rdp_unavailable"})
		}
		return
	}
	if parent.Err() != nil {
		_ = conn.Close()
		return
	}
	s := &tcpStream{conn: conn, queue: make(chan []byte, 16), done: make(chan struct{})}
	m.mu.Lock()
	if parent.Err() != nil || len(m.streams) >= 16 || m.streams[id] != nil {
		m.mu.Unlock()
		_ = conn.Close()
		return
	}
	m.streams[id] = s
	m.mu.Unlock()
	if m.send(message{Version: 1, Type: "tcp_opened", SessionID: id}) != nil {
		m.close(id, s)
		return
	}
	go func() {
		for {
			select {
			case data := <-s.queue:
				_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if _, err := s.conn.Write(data); err != nil {
					m.close(id, s)
					return
				}
			case <-s.done:
				return
			}
		}
	}()
	go func() {
		defer m.close(id, s)
		defer m.send(message{Version: 1, Type: "tcp_close", SessionID: id})
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if n > 0 && m.send(message{Version: 1, Type: "tcp_data", SessionID: id, Data: base64.StdEncoding.EncodeToString(buf[:n])}) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
}

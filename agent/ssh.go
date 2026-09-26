package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

type sshStream struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	client  *ssh.Client
	session *ssh.Session
	stdin   io.WriteCloser
	closed  chan struct{}
	once    sync.Once
}

type sshMux struct {
	conn    *websocket.Conn
	// address is set only by in-process tests; production always uses localhost:22.
	address string
	writeMu sync.Mutex
	mu      sync.Mutex
	streams map[string]*sshStream
}

func (m *sshMux) send(msg message) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	m.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return m.conn.WriteJSON(msg)
}

func (m *sshMux) close(id string, stream *sshStream) {
	stream.once.Do(func() {
		close(stream.closed)
		stream.cancel()
		m.mu.Lock()
		if m.streams[id] == stream {
			delete(m.streams, id)
		}
		m.mu.Unlock()
		stream.mu.Lock()
		if stream.session != nil {
			_ = stream.session.Close()
		}
		if stream.client != nil {
			_ = stream.client.Close()
		}
		stream.mu.Unlock()
	})
}

func (m *sshMux) closeAll() {
	m.mu.Lock()
	copy := make(map[string]*sshStream, len(m.streams))
	for id, stream := range m.streams {
		copy[id] = stream
	}
	m.mu.Unlock()
	for id, stream := range copy {
		m.close(id, stream)
	}
}

func sshFailure(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timeout") {
		return "timeout"
	}
	if strings.Contains(err.Error(), "host key mismatch") {
		return "host_key_mismatch"
	}
	if strings.Contains(err.Error(), "unable to authenticate") {
		return "authentication_failed"
	}
	return "ssh_unavailable"
}

func sshConfig(msg message, secret []byte, fingerprint *string) (*ssh.ClientConfig, error) {
	var auth ssh.AuthMethod
	switch msg.Kind {
	case "linux_password":
		auth = ssh.Password(string(secret))
	case "linux_ssh_key":
		signer, err := ssh.ParsePrivateKey(secret)
		if err != nil {
			return nil, err
		}
		auth = ssh.PublicKeys(signer)
	default:
		return nil, errors.New("unsupported credential")
	}
	return &ssh.ClientConfig{
		User: msg.Username, Auth: []ssh.AuthMethod{auth}, Timeout: 10 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			*fingerprint = ssh.FingerprintSHA256(key)
			if msg.HostKey != "" && msg.HostKey != *fingerprint {
				return errors.New("host key mismatch")
			}
			return nil
		},
	}, nil
}

func (m *sshMux) open(msg message) {
	secret, err := base64.StdEncoding.DecodeString(msg.Secret)
	msg.Secret = ""
	if err != nil || len(secret) == 0 || len(secret) > 16384 || msg.Username == "" || msg.Columns < 20 || msg.Columns > 500 || msg.Rows < 5 || msg.Rows > 200 {
		_ = m.send(message{Version: 1, Type: "session_error", SessionID: msg.SessionID, Code: "unsupported_credential"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &sshStream{cancel: cancel, closed: make(chan struct{})}
	m.mu.Lock()
	if m.streams == nil {
		m.streams = make(map[string]*sshStream)
	}
	if m.streams[msg.SessionID] != nil || len(m.streams) >= 16 {
		m.mu.Unlock()
		cancel()
		for i := range secret {
			secret[i] = 0
		}
		_ = m.send(message{Version: 1, Type: "session_error", SessionID: msg.SessionID, Code: "agent_unavailable"})
		return
	}
	m.streams[msg.SessionID] = stream
	m.mu.Unlock()
	go func() {
		defer func() {
			for i := range secret {
				secret[i] = 0
			}
		}()
		handshakeCtx, handshakeCancel := context.WithTimeout(ctx, 15*time.Second)
		defer handshakeCancel()
		var fingerprint string
		config, err := sshConfig(msg, secret, &fingerprint)
		if err != nil {
			m.fail(msg.SessionID, stream, "unsupported_credential")
			return
		}
		// The target-side SSH endpoint is hard-coded; no host or port arrives from
		// the browser or broker protocol.
		address := m.address
		if address == "" {
			address = "127.0.0.1:22"
		}
		netConn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(handshakeCtx, "tcp", address)
		if err != nil {
			m.fail(msg.SessionID, stream, sshFailure(err))
			return
		}
		handshakeDone := make(chan struct{})
		go func() {
			select {
			case <-handshakeCtx.Done():
				_ = netConn.Close()
			case <-handshakeDone:
			}
		}()
		transport, channels, requests, err := ssh.NewClientConn(netConn, address, config)
		close(handshakeDone)
		for i := range secret {
			secret[i] = 0
		}
		config.Auth = nil
		if err != nil {
			_ = netConn.Close()
			m.fail(msg.SessionID, stream, sshFailure(err))
			return
		}
		client := ssh.NewClient(transport, channels, requests)
		stream.mu.Lock()
		select {
		case <-stream.closed:
			stream.mu.Unlock()
			_ = client.Close()
			return
		default:
		}
		stream.client = client
		stream.mu.Unlock()
		session, err := client.NewSession()
		if err != nil {
			m.fail(msg.SessionID, stream, "ssh_unavailable")
			return
		}
		stream.mu.Lock()
		select {
		case <-stream.closed:
			stream.mu.Unlock()
			_ = session.Close()
			return
		default:
		}
		stream.session = session
		stream.mu.Unlock()
		stdin, err := session.StdinPipe()
		if err != nil {
			m.fail(msg.SessionID, stream, "ssh_unavailable")
			return
		}
		stream.mu.Lock()
		stream.stdin = stdin
		stream.mu.Unlock()
		stdout, err := session.StdoutPipe()
		if err != nil {
			m.fail(msg.SessionID, stream, "ssh_unavailable")
			return
		}
		stderr, err := session.StderrPipe()
		if err != nil {
			m.fail(msg.SessionID, stream, "ssh_unavailable")
			return
		}
		if err = session.RequestPty("xterm-256color", msg.Rows, msg.Columns, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
			m.fail(msg.SessionID, stream, "ssh_unavailable")
			return
		}
		if err = session.Shell(); err != nil {
			m.fail(msg.SessionID, stream, "ssh_unavailable")
			return
		}
		if err = m.send(message{Version: 1, Type: "session_opened", SessionID: msg.SessionID, Fingerprint: fingerprint}); err != nil {
			m.close(msg.SessionID, stream)
			return
		}
		go m.pump(msg.SessionID, stream, stdout)
		go m.pump(msg.SessionID, stream, stderr)
		_ = session.Wait()
		select {
		case <-stream.closed:
		default:
			_ = m.send(message{Version: 1, Type: "session_close", SessionID: msg.SessionID})
		}
		m.close(msg.SessionID, stream)
	}()
}

func (m *sshMux) fail(id string, stream *sshStream, code string) {
	select {
	case <-stream.closed:
	default:
		_ = m.send(message{Version: 1, Type: "session_error", SessionID: id, Code: code})
	}
	m.close(id, stream)
}

func (m *sshMux) pump(id string, stream *sshStream, reader io.Reader) {
	buffer := make([]byte, 4096)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if m.send(message{Version: 1, Type: "session_data", SessionID: id, Data: base64.StdEncoding.EncodeToString(buffer[:n])}) != nil {
				m.close(id, stream)
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (m *sshMux) handle(msg message) bool {
	if len(msg.SessionID) != 36 {
		return false
	}
	if msg.Type == "session_open" {
		m.open(msg)
		return true
	}
	m.mu.Lock()
	stream := m.streams[msg.SessionID]
	m.mu.Unlock()
	if stream == nil {
		return true
	} // delayed close from an already ended stream
	switch msg.Type {
	case "session_data":
		data, err := base64.StdEncoding.DecodeString(msg.Data)
		if err != nil || len(data) > 8192 {
			return false
		}
		stream.mu.Lock()
		stdin := stream.stdin
		stream.mu.Unlock()
		if stdin != nil {
			if _, err := stdin.Write(data); err != nil {
				m.close(msg.SessionID, stream)
			}
		}
	case "session_resize":
		if msg.Columns < 20 || msg.Columns > 500 || msg.Rows < 5 || msg.Rows > 200 {
			return false
		}
		stream.mu.Lock()
		session := stream.session
		stream.mu.Unlock()
		if session != nil {
			_ = session.WindowChange(msg.Rows, msg.Columns)
		}
	case "session_close":
		m.close(msg.SessionID, stream)
	default:
		return false
	}
	return true
}

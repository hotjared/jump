package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSSHTerminalCapability(t *testing.T) {
	capabilities := info().Capabilities
	has := func(name string) bool {
		for _, capability := range capabilities {
			if capability == name {
				return true
			}
		}
		return false
	}
	if has("ssh_terminal_v1") != (runtime.GOOS == "linux") {
		t.Fatalf("unexpected SSH terminal capability on %s: %v", runtime.GOOS, capabilities)
	}
	if runtime.GOOS == "linux" && !has("ssh") {
		t.Fatal("generic SSH capability was removed")
	}
}

func TestSSHConfigPasswordKeyAndHostTrust(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := ssh.FingerprintSHA256(key)
	msg := message{Kind: "linux_password", Username: "other-user", HostKey: fingerprint}
	var reported string
	config, err := sshConfig(msg, []byte("password"), &reported)
	if err != nil || config.User != "other-user" || len(config.Auth) != 1 {
		t.Fatalf("password setup: %v", err)
	}
	if err := config.HostKeyCallback("127.0.0.1:22", nil, key); err != nil || reported != fingerprint {
		t.Fatalf("host trust: %v", err)
	}
	msg.HostKey = "SHA256:changed"
	config, _ = sshConfig(msg, []byte("password"), &reported)
	if err := config.HostKeyCallback("127.0.0.1:22", nil, key); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatal("mismatch accepted")
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	msg.Kind, msg.HostKey = "linux_ssh_key", ""
	config, err = sshConfig(msg, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), &reported)
	if err != nil || len(config.Auth) != 1 {
		t.Fatalf("key setup: %v", err)
	}
	if err := config.HostKeyCallback("127.0.0.1:22", nil, key); err != nil || reported != fingerprint {
		t.Fatalf("TOFU callback: %v", err)
	}
	if _, err := sshConfig(msg, []byte("not a key"), &reported); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestStreamIsolationAndResizeValidation(t *testing.T) {
	m := &sshMux{streams: make(map[string]*sshStream)}
	id := "4af3210f-5430-4db0-85b2-8a8560d23420"
	if m.handle(message{Type: "session_resize", SessionID: "invalid", Rows: 24, Columns: 80}) {
		t.Fatal("invalid id accepted")
	}
	if !m.handle(message{Type: "session_close", SessionID: id}) {
		t.Fatal("late close rejected")
	}
	stream := &sshStream{cancel: func() {}, closed: make(chan struct{})}
	m.streams[id] = stream
	if m.handle(message{Type: "session_resize", SessionID: id, Rows: 0, Columns: 80}) {
		t.Fatal("invalid resize accepted")
	}
	if m.handle(message{Type: "session_data", SessionID: id, Data: "!!!"}) {
		t.Fatal("malformed data accepted")
	}
	if !m.handle(message{Type: "session_close", SessionID: id}) {
		t.Fatal("close rejected")
	}
	if len(m.streams) != 0 {
		t.Fatal("stream not cleaned up")
	}
}

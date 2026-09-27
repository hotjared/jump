package main

import (
	"encoding/base64"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestRDPCapabilityAndLinuxRejection(t *testing.T) {
	has := false
	for _, c := range info().Capabilities {
		if c == "rdp_tunnel_v1" {
			has = true
		}
	}
	if has != (runtime.GOOS == "windows") {
		t.Fatalf("unexpected RDP capability on %s", runtime.GOOS)
	}
	if runtime.GOOS == "windows" {
		return
	}
	frames := make(chan message, 1)
	m := &tcpMux{send: func(msg message) error { frames <- msg; return nil }}
	if !m.handle(message{Type: "tcp_open", SessionID: "29bcac87-43b5-48cf-a4a1-ea46c49f2a82"}) || (<-frames).Code != "unsupported_agent" {
		t.Fatal("Linux did not reject RDP")
	}
}

func TestTCPStreamLocalTransportAndCleanup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 16)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					_, _ = conn.Write(buf[:n])
				}
			}()
		}
	}()
	frames := make(chan message, 100)
	m := &tcpMux{testWindows: true, address: listener.Addr().String(), streams: make(map[string]*tcpStream), send: func(msg message) error { frames <- msg; return nil }}
	ids := []string{"29bcac87-43b5-48cf-a4a1-ea46c49f2a82", "a4bfc02e-d8c4-4c4e-8c79-070e3714ed2f"}
	for _, id := range ids {
		m.handle(message{Type: "tcp_open", SessionID: id})
	}
	opened := map[string]bool{}
	for len(opened) < 2 {
		select {
		case frame := <-frames:
			if frame.Type != "tcp_opened" {
				t.Fatalf("open: %+v", frame)
			}
			opened[frame.SessionID] = true
		case <-time.After(3 * time.Second):
			t.Fatal("open timed out")
		}
	}
	for _, id := range ids {
		m.handle(message{Type: "tcp_data", SessionID: id, Data: base64.StdEncoding.EncodeToString([]byte(id[:4]))})
	}
	echoed := map[string]bool{}
	for len(echoed) < 2 {
		select {
		case frame := <-frames:
			if frame.Type != "tcp_data" {
				t.Fatalf("data: %+v", frame)
			}
			data, _ := base64.StdEncoding.DecodeString(frame.Data)
			if string(data) != frame.SessionID[:4] {
				t.Fatal("stream data crossed devices")
			}
			echoed[frame.SessionID] = true
		case <-time.After(3 * time.Second):
			t.Fatal("echo timed out")
		}
	}
	if !m.handle(message{Type: "tcp_data", SessionID: ids[0], Data: "!!!"}) {
		t.Fatal("malformed frame rejected without cleanup")
	}
	m.closeAll()
	m.mu.Lock()
	remaining := len(m.streams)
	m.mu.Unlock()
	if remaining != 0 {
		t.Fatal("disconnect left streams open")
	}
}

func TestTCPConnectionRefusedAndFrameLimit(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	address := l.Addr().String()
	l.Close()
	var mu sync.Mutex
	var frames []message
	m := &tcpMux{testWindows: true, address: address, streams: make(map[string]*tcpStream), send: func(msg message) error { mu.Lock(); frames = append(frames, msg); mu.Unlock(); return nil }}
	id := "29bcac87-43b5-48cf-a4a1-ea46c49f2a82"
	m.handle(message{Type: "tcp_open", SessionID: id})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(frames)
		if count > 0 && frames[0].Code != "rdp_unavailable" {
			t.Fatal(frames[0])
		}
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	count := len(frames)
	mu.Unlock()
	if count != 1 {
		t.Fatal("refused connection did not report failure")
	}
	if m.handle(message{Type: "tcp_close", SessionID: "invalid"}) == false {
		t.Fatal("invalid frame should be dropped safely")
	}
}

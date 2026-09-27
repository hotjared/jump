package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// Exercises the production TCP mux against a real socket. Only the destination
// is injected; production still uses the fixed loopback RDP address.
func TestTCPMuxPreservesFragmentedBidirectionalBinaryStream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	toWindows := bytes.Repeat([]byte{0, 255, 1, 254, 13, 10, 0, 42}, 32769)
	toGuacd := bytes.Repeat([]byte{255, 0, 253, 2, 10, 13, 99}, 37451)
	serverResult := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverResult <- nil
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		written := make(chan struct{})
		go func() {
			defer close(written)
			for offset := 0; offset < len(toGuacd); {
				end := min(offset+8192, len(toGuacd))
				if _, err := conn.Write(toGuacd[offset:end]); err != nil {
					return
				}
				offset = end
			}
		}()
		got := make([]byte, len(toWindows))
		_, err = io.ReadFull(conn, got)
		<-written
		if err != nil {
			serverResult <- nil
			return
		}
		serverResult <- got
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	frames := make(chan message, 1024)
	m := &tcpMux{
		testWindows: true, address: listener.Addr().String(),
		streams: make(map[string]*tcpStream),
		send:    func(frame message) error { frames <- frame; return nil },
	}
	defer m.closeAll()
	id := "29bcac87-43b5-48cf-a4a1-ea46c49f2a82"
	m.handle(message{Type: "tcp_open", SessionID: id})
	select {
	case frame := <-frames:
		if frame.Type != "tcp_opened" {
			t.Fatalf("open: %s", frame.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TCP open timed out")
	}
	var sender sync.WaitGroup
	sender.Add(1)
	go func() {
		defer sender.Done()
		for offset, n := 0, 0; offset < len(toWindows); n++ {
			size := 37
			if n%17 == 0 {
				size = 8192
			}
			end := min(offset+size, len(toWindows))
			m.handle(message{Type: "tcp_data", SessionID: id,
				Data: base64.StdEncoding.EncodeToString(toWindows[offset:end])})
			offset = end
			// A real broker applies backpressure between WebSocket writes.
			time.Sleep(100 * time.Microsecond)
		}
	}()
	deadline := time.After(15 * time.Second)
	got := make([]byte, 0, len(toGuacd))
	for {
		select {
		case frame := <-frames:
			switch frame.Type {
			case "tcp_data":
				data, err := base64.StdEncoding.DecodeString(frame.Data)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, data...)
			case "tcp_close":
				sender.Wait()
				if !bytes.Equal(got, toGuacd) {
					t.Fatalf("Windows -> guacd stream differs: %d of %d bytes", len(got), len(toGuacd))
				}
				if received := <-serverResult; !bytes.Equal(received, toWindows) {
					t.Fatalf("guacd -> Windows stream differs: %d of %d bytes", len(received), len(toWindows))
				}
				return
			default:
				t.Fatalf("unexpected frame: %s", frame.Type)
			}
		case <-deadline:
			t.Fatal("bidirectional stream timed out")
		}
	}
}

func TestTCPMuxLocalCloseReachesTarget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			closed <- err
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = io.Copy(io.Discard, conn)
		closed <- err
	}()
	frames := make(chan message, 16)
	m := &tcpMux{testWindows: true, address: listener.Addr().String(), streams: make(map[string]*tcpStream),
		send: func(frame message) error { frames <- frame; return nil }}
	defer m.closeAll()
	id := "29bcac87-43b5-48cf-a4a1-ea46c49f2a82"
	m.handle(message{Type: "tcp_open", SessionID: id})
	select {
	case frame := <-frames:
		if frame.Type != "tcp_opened" {
			t.Fatalf("open: %s", frame.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TCP open timed out")
	}
	m.handle(message{Type: "tcp_close", SessionID: id})
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("target did not observe EOF: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local close did not reach target")
	}
}

package main

import (
	"context"
	"testing"
	"time"
)

func TestScreenCloseAcknowledgesReleasedHelperAndAllowsReplacement(t *testing.T) {
	desktop := &fakeDesktop{frames: make(chan desktopFrame), inputs: make(chan screenInput, 1), closed: make(chan struct{})}
	sent := make(chan message, 8)
	var mux *screenMux
	mux = &screenMux{
		launch: func(context.Context) (desktopBridge, error) { return desktop, nil },
		send: func(frame message) error {
			if frame.Type == "screen_close" {
				mux.mu.Lock()
				busy := mux.stream != nil
				mux.mu.Unlock()
				if busy {
					t.Error("close acknowledged while controller still reserved")
				}
				select {
				case <-desktop.closed:
				default:
					t.Error("helper not closed before acknowledgement")
				}
			}
			sent <- frame
			return nil
		},
	}
	defer mux.closeAll()
	mux.handle(message{Type: "screen_open", SessionID: screenTestID})
	select {
	case frame := <-sent:
		if frame.Type != "screen_opened" {
			t.Fatal(frame)
		}
	case <-time.After(time.Second):
		t.Fatal("open timed out")
	}
	mux.handle(message{Type: "screen_close", SessionID: screenTestID})
	if frame := <-sent; frame.Type != "screen_close" {
		t.Fatal(frame)
	}
	mux.handle(message{Type: "screen_close", SessionID: screenTestID})
	if frame := <-sent; frame.Type != "screen_close" {
		t.Fatal("repeat close not acknowledged")
	}
	mux.handle(message{Type: "screen_open", SessionID: "ad0b50e5-4cad-4fcf-9666-3bf2f8e43b4c"})
	select {
	case frame := <-sent:
		if frame.Type != "screen_opened" {
			t.Fatal(frame)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement blocked")
	}
}

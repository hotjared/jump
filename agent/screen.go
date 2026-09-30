package main

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"time"
)

type desktopFrame struct {
	JPEG   []byte
	Width  int
	Height int
}
type desktopBridge interface {
	Next(context.Context) (desktopFrame, error)
	Input(screenInput) error
	Close()
}

type screenStream struct {
	ctx      context.Context
	cancel   context.CancelFunc
	input    chan message
	ack      chan uint64
	closed   chan struct{}
	finished chan struct{}
	once     sync.Once
}
type screenMux struct {
	mu     sync.Mutex
	stream *screenStream
	id     string
	send   func(message) error
	launch func(context.Context) (desktopBridge, error)
}

func (m *screenMux) close(s *screenStream) {
	s.once.Do(func() {
		s.cancel()
		close(s.closed)
	})
}
func (m *screenMux) closeAll() {
	m.mu.Lock()
	s := m.stream
	m.mu.Unlock()
	if s != nil {
		m.close(s)
		<-s.finished
	}
}
func (m *screenMux) handle(msg message) bool {
	if !strings.HasPrefix(msg.Type, "screen_") {
		return false
	}
	if !validScreenID(msg.SessionID) {
		return true
	}
	m.mu.Lock()
	s, id := m.stream, m.id
	m.mu.Unlock()
	if msg.Type == "screen_open" {
		if s != nil {
			_ = m.send(message{Version: 1, Type: "screen_error", SessionID: msg.SessionID, Code: "screen_busy"})
			return true
		}
		ctx, cancel := context.WithCancel(context.Background())
		s = &screenStream{ctx: ctx, cancel: cancel, input: make(chan message, 64), ack: make(chan uint64, 1), closed: make(chan struct{}), finished: make(chan struct{})}
		m.mu.Lock()
		if m.stream != nil {
			m.mu.Unlock()
			cancel()
			return true
		}
		m.stream, m.id = s, msg.SessionID
		m.mu.Unlock()
		go m.run(s, msg.SessionID)
		return true
	}
	if s == nil || id != msg.SessionID {
		return true
	}
	switch msg.Type {
	case "screen_close":
		m.close(s)
	case "screen_ack":
		select {
		case s.ack <- msg.FrameID:
		default:
			m.close(s)
		}
	case "screen_mode", "screen_input":
		if msg.Type == "screen_mode" && msg.Mode != "control" && msg.Mode != "view" || msg.Type == "screen_input" && !validScreenInput(msg.Input) {
			m.close(s)
			return true
		}
		select {
		case s.input <- msg:
		default:
			m.close(s)
		}
	default:
		m.close(s)
	}
	return true
}
func (m *screenMux) run(s *screenStream, id string) {
	defer func() {
		m.close(s)
		m.mu.Lock()
		if m.stream == s {
			m.stream = nil
			m.id = ""
		}
		m.mu.Unlock()
		close(s.finished)
	}()
	fail := func(code string) { _ = m.send(message{Version: 1, Type: "screen_error", SessionID: id, Code: code}) }
	bridge, err := m.launch(s.ctx)
	if err != nil {
		fail(screenFailure(err))
		return
	}
	defer bridge.Close()
	if s.ctx.Err() != nil {
		return
	}
	if m.send(message{Version: 1, Type: "screen_opened", SessionID: id}) != nil {
		return
	}
	frames := make(chan desktopFrame, 1)
	errors := make(chan error, 1)
	readerDone := make(chan struct{})
	defer func() { s.cancel(); <-readerDone }()
	go func() {
		defer close(readerDone)
		for {
			frame, err := bridge.Next(s.ctx)
			if err != nil {
				select {
				case errors <- err:
				case <-s.ctx.Done():
				}
				return
			}
			select {
			case frames <- frame:
			case <-s.ctx.Done():
				return
			}
		}
	}()
	mode := "control"
	var frameID uint64
	var waiting uint64
	timeout := time.NewTimer(time.Hour)
	defer timeout.Stop()
	timeout.Stop()
	for {
		var capture <-chan desktopFrame
		if waiting == 0 {
			capture = frames
		}
		select {
		case <-s.ctx.Done():
			return
		case err := <-errors:
			fail(screenFailure(err))
			return
		case <-timeout.C:
			fail("session_timeout")
			return
		case ack := <-s.ack:
			if ack != waiting || waiting == 0 {
				fail("invalid_frame")
				return
			}
			waiting = 0
			timeout.Stop()
		case in := <-s.input:
			if s.ctx.Err() != nil {
				return
			}
			if in.Type == "screen_mode" {
				if err := bridge.Input(screenInput{Action: "release"}); err != nil {
					fail("input_failed")
					return
				}
				mode = in.Mode
				if m.send(message{Version: 1, Type: "screen_mode", SessionID: id, Mode: mode}) != nil {
					return
				}
			} else if mode == "control" {
				if err := bridge.Input(*in.Input); err != nil {
					fail("input_failed")
					return
				}
			}
		case frame := <-capture:
			if len(frame.JPEG) == 0 || len(frame.JPEG) > screenMaxFrame || frame.Width < 1 || frame.Width > screenMaxWidth || frame.Height < 1 || frame.Height > screenMaxHeight {
				fail("capture_failed")
				return
			}
			frameID++
			count := (len(frame.JPEG) + screenChunkBytes - 1) / screenChunkBytes
			for i := 0; i < count; i++ {
				if s.ctx.Err() != nil {
					return
				}
				end := (i + 1) * screenChunkBytes
				if end > len(frame.JPEG) {
					end = len(frame.JPEG)
				}
				if m.send(message{Version: 1, Type: "screen_frame", SessionID: id, FrameID: frameID, Index: i, Count: count, Width: frame.Width, Height: frame.Height, Data: base64.StdEncoding.EncodeToString(frame.JPEG[i*screenChunkBytes : end])}) != nil {
					return
				}
			}
			waiting = frameID
			timeout.Reset(5 * time.Second)
		}
	}
}
func screenFailure(err error) string {
	if safeCaptureCode(err.Error()) {
		return err.Error()
	}
	switch err.Error() {
	case "no_interactive_session", "helper_start_failed", "capture_failed", "input_failed", "interactive_session_changed", "secure_desktop":
		return err.Error()
	}
	return "capture_failed"
}

// Session discovery and the platform decision stay testable without a desktop.
func screenPlatformSupported(goos, arch string, major uint32, system bool) bool {
	return goos == "windows" && arch == "amd64" && major >= 10 && system
}
func discoverDesktopSession(discover func() uint32, loggedIn func(uint32) bool) (uint32, bool) {
	id := discover()
	if id == 0 || id == 0xffffffff || !loggedIn(id) {
		return 0, false
	}
	return id, true
}

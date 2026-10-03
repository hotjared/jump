//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

var screenUser32 = windows.NewLazySystemDLL("user32.dll")
var screenGDI32 = windows.NewLazySystemDLL("gdi32.dll")

func screenSupported() bool {
	u, e := windows.GetCurrentProcessToken().GetTokenUser()
	return screenPlatformSupported(runtime.GOOS, runtime.GOARCH, windows.RtlGetVersion().MajorVersion, e == nil && u.User.Sid.IsWellKnown(windows.WinLocalSystemSid))
}

type desktopPipe struct {
	ctx     context.Context
	handle  windows.Handle
	writeMu sync.Mutex
}

func (p *desktopPipe) Read(b []byte) (int, error) {
	for {
		if e := p.ctx.Err(); e != nil {
			return 0, e
		}
		var n uint32
		e := windows.ReadFile(p.handle, b, &n, nil)
		if n > 0 {
			return int(n), nil
		}
		if e != nil && e != windows.ERROR_NO_DATA && e != windows.ERROR_PIPE_LISTENING {
			return 0, e
		}
		select {
		case <-p.ctx.Done():
			return 0, p.ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (p *desktopPipe) Write(b []byte) (int, error) {
	total := 0
	deadline := time.Now().Add(3 * time.Second)
	for len(b) > 0 {
		if e := p.ctx.Err(); e != nil {
			return total, e
		}
		var n uint32
		// PIPE_NOWAIT can make no progress on a write larger than the pipe's
		// quota, even with a reader waiting. JSON/base64 desktop frames routinely
		// exceed the 64 KiB pipe buffer. Stream bounded writes through the byte
		// pipe; preserve partial-write accounting and cancellation between chunks.
		chunk := b[:min(len(b), screenChunkBytes)]
		e := windows.WriteFile(p.handle, chunk, &n, nil)
		if e != nil && e != windows.ERROR_NO_DATA {
			return total, e
		}
		total += int(n)
		b = b[n:]
		if n == 0 {
			if time.Now().After(deadline) {
				return total, errors.New("session_timeout")
			}
			select {
			case <-p.ctx.Done():
				return total, p.ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	return total, nil
}
func (p *desktopPipe) send(v any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	b, e := json.Marshal(v)
	if e != nil || len(b) > screenMaxFrame*2 {
		return errors.New("invalid_frame")
	}
	var h [4]byte
	binary.LittleEndian.PutUint32(h[:], uint32(len(b)))
	if _, e = p.Write(h[:]); e != nil {
		return e
	}
	_, e = p.Write(b)
	return e
}
func (p *desktopPipe) receive(v any) error {
	var h [4]byte
	if _, e := io.ReadFull(p, h[:]); e != nil {
		return e
	}
	n := binary.LittleEndian.Uint32(h[:])
	if n == 0 || n > screenMaxFrame*2 {
		return errors.New("invalid_frame")
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(p, b); e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}

type helperFrame struct {
	Data      []byte   `json:"data,omitempty"`
	Width     int      `json:"width,omitempty"`
	Height    int      `json:"height,omitempty"`
	Code      string   `json:"code,omitempty"`
	Released  bool     `json:"released,omitempty"`
	Operation *message `json:"operation,omitempty"`
}
type windowsDesktop struct {
	frames        chan desktopFrame
	errors        chan error
	readerDone    chan struct{}
	cancel        context.CancelFunc
	pipe          *desktopPipe
	job           windows.Handle
	session       uint32
	once          sync.Once
	events        chan message
	results       chan message
	operationMu   sync.Mutex
	resultMu      sync.Mutex
	resultID      string
	resultContext context.Context
}

func (d *windowsDesktop) Next(ctx context.Context) (desktopFrame, error) {
	select {
	case frame := <-d.frames:
		return frame, nil
	case err := <-d.errors:
		return desktopFrame{}, err
	case <-ctx.Done():
		return desktopFrame{}, ctx.Err()
	case <-d.pipe.ctx.Done():
		return desktopFrame{}, d.pipe.ctx.Err()
	}
}
func (d *windowsDesktop) readIPC() {
	defer close(d.readerDone)
	if err := d.pumpIPC(); err != nil {
		select {
		case d.errors <- err:
		case <-d.pipe.ctx.Done():
		}
	}
}
func (d *windowsDesktop) pumpIPC() error {
	for {
		var f helperFrame
		if e := d.pipe.receive(&f); e != nil {
			return e
		}
		if f.Operation != nil {
			if f.Operation.Type == "screen_event" {
				select {
				case d.events <- *f.Operation:
				default:
				}
			} else {
				d.resultMu.Lock()
				id, ctx := d.resultID, d.resultContext
				d.resultMu.Unlock()
				if ctx != nil && id == f.Operation.RequestID {
					select {
					case d.results <- *f.Operation:
					case <-ctx.Done():
					case <-d.pipe.ctx.Done():
						return d.pipe.ctx.Err()
					}
				}
			}
			continue
		}
		if f.Released {
			continue
		}
		if f.Code != "" {
			return errors.New(f.Code)
		}
		if windows.WTSGetActiveConsoleSessionId() != d.session {
			return errors.New("interactive_session_changed")
		}
		frame := desktopFrame{f.Data, f.Width, f.Height}
		// Reading IPC must not stall clipboard replies behind frame credit. Retain
		// only the latest unconsumed capture; mux still assigns IDs and requires ACK.
		select {
		case d.frames <- frame:
		default:
			select {
			case <-d.frames:
			default:
			}
			select {
			case d.frames <- frame:
			case <-d.pipe.ctx.Done():
				return d.pipe.ctx.Err()
			}
		}
	}
}
func (d *windowsDesktop) Input(in screenInput) error {
	if !validScreenInput(&in) || windows.WTSGetActiveConsoleSessionId() != d.session {
		return errors.New("input_failed")
	}
	return d.pipe.send(message{Type: "screen_input", Input: &in})
}
func (d *windowsDesktop) Close() {
	d.once.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
		if d.readerDone != nil {
			<-d.readerDone
		}
		d.pipe.writeMu.Lock()
		defer d.pipe.writeMu.Unlock()
		// Capture reader has stopped before Close. Give the helper a bounded chance
		// to release its own injected keys/buttons before enforcing job termination.
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		pipe := &desktopPipe{ctx: ctx, handle: d.pipe.handle}
		if pipe.send(message{Type: "screen_input", Input: &screenInput{Action: "release"}}) == nil {
			for {
				var f helperFrame
				if pipe.receive(&f) != nil || f.Released {
					break
				}
			}
		}
		windows.CloseHandle(d.job)
		windows.CloseHandle(d.pipe.handle)
	})
}
func launchConsoleHelper(ctx context.Context, events chan message) (*windowsDesktop, error) {
	if !screenSupported() {
		return nil, errors.New("helper_start_failed")
	}
	session, found := discoverDesktopSession(windows.WTSGetActiveConsoleSessionId, func(id uint32) bool {
		// Query session existence/state, never a logged-on user's token. WTSInit is
		// valid during sign-in; a disconnected/nonexistent session is not console.
		var info *byte
		var size uint32
		r, _, _ := windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW").Call(0, uintptr(id), 8, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&size)))
		if r == 0 || info == nil {
			return false
		}
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(info)))
		if size < 4 {
			return false
		}
		state := *(*uint32)(unsafe.Pointer(info))
		return state == 0 || state == 1 || state == 9 // Active, Connected, Init
	})
	if !found {
		return nil, errors.New("no_interactive_session")
	}
	var random [16]byte
	if _, e := rand.Read(random[:]); e != nil {
		return nil, errors.New("helper_start_failed")
	}
	name := `\\.\pipe\jump-desktop-` + hex.EncodeToString(random[:])
	sd, e := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)")
	if e != nil {
		return nil, errors.New("helper_start_failed")
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	pipe, e := windows.CreateNamedPipe(windows.StringToUTF16Ptr(name), windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE, windows.PIPE_TYPE_BYTE|windows.PIPE_NOWAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 65536, 65536, 0, &sa)
	if e != nil {
		return nil, errors.New("helper_start_failed")
	}
	job, e := windows.CreateJobObject(nil, nil)
	if e != nil {
		windows.CloseHandle(pipe)
		return nil, errors.New("helper_start_failed")
	}
	pipeCtx, pipeCancel := context.WithCancel(ctx)
	d := &windowsDesktop{cancel: pipeCancel, pipe: &desktopPipe{ctx: pipeCtx, handle: pipe}, job: job, session: session, events: events, results: make(chan message, 1)}
	ok := false
	defer func() {
		if !ok {
			d.Close()
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, e = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); e != nil {
		return nil, errors.New("helper_start_failed")
	}
	var source windows.Token
	if windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &source) != nil {
		return nil, errors.New("helper_start_failed")
	}
	defer source.Close()
	var token windows.Token
	if windows.DuplicateTokenEx(source, windows.TOKEN_ALL_ACCESS, nil, windows.SecurityImpersonation, windows.TokenPrimary, &token) != nil {
		return nil, errors.New("helper_start_failed")
	}
	defer token.Close()
	if windows.SetTokenInformation(token, windows.TokenSessionId, (*byte)(unsafe.Pointer(&session)), 4) != nil {
		return nil, errors.New("helper_start_failed")
	}
	// Fixed protected installed executable and desktop; nothing remotely supplied selects a local resource.
	exe := windowsInstallPath()
	command := fmt.Sprintf("%q internal-desktop-helper %s %d %d", exe, name, os.Getpid(), session)
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Desktop: windows.StringToUTF16Ptr(`winsta0\default`)}
	var pi windows.ProcessInformation
	env := windows.StringToUTF16("SystemRoot=" + os.Getenv("SystemRoot"))
	env = append(env, 0)
	if windows.CreateProcessAsUser(token, windows.StringToUTF16Ptr(exe), windows.StringToUTF16Ptr(command), nil, nil, false, windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW, &env[0], nil, &si, &pi) != nil {
		return nil, errors.New("helper_start_failed")
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	if windows.AssignProcessToJobObject(job, pi.Process) != nil {
		windows.TerminateProcess(pi.Process, 1)
		return nil, errors.New("helper_start_failed")
	}
	if _, e = windows.ResumeThread(pi.Thread); e != nil {
		return nil, errors.New("helper_start_failed")
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		e = windows.ConnectNamedPipe(pipe, nil)
		if e == nil || e == windows.ERROR_PIPE_CONNECTED {
			var client uint32
			if windows.GetNamedPipeClientProcessId(pipe, &client) == nil {
				if client != pi.ProcessId {
					return nil, errors.New("helper_start_failed")
				}
				break
			}
			e = windows.ERROR_PIPE_LISTENING
		}
		if e != windows.ERROR_PIPE_LISTENING || time.Now().After(deadline) {
			return nil, errors.New("helper_start_failed")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	var pid uint32
	if windows.GetNamedPipeClientProcessId(pipe, &pid) != nil || pid != pi.ProcessId {
		return nil, errors.New("helper_start_failed")
	}
	d.frames = make(chan desktopFrame, 1)
	d.errors = make(chan error, 1)
	d.readerDone = make(chan struct{})
	go d.readIPC()
	ok = true
	return d, nil
}
func openScreenDesktop(trace *screenRuntimeTrace) (uintptr, string) {
	h, _, e := screenUser32.NewProc("OpenInputDesktop").Call(0, 0, uintptr(screenInputDesktopAccess()))
	trace.record("desktop_open", "", h != 0, screenWin32Code(e), "")
	if h == 0 {
		return 0, ""
	}
	var name [256]uint16
	var size uint32
	r, _, e := screenUser32.NewProc("GetUserObjectInformationW").Call(h, 2, uintptr(unsafe.Pointer(&name[0])), 512, uintptr(unsafe.Pointer(&size)))
	trace.record("desktop_name", windows.UTF16ToString(name[:]), r != 0, screenWin32Code(e), "")
	if r == 0 {
		screenUser32.NewProc("CloseDesktop").Call(h)
		return 0, ""
	}
	return h, windows.UTF16ToString(name[:])
}
func desktopHelper(args []string) error {
	if len(args) != 3 || !screenSupported() || !strings.HasPrefix(args[0], `\\.\pipe\jump-desktop-`) || len(args[0]) != len(`\\.\pipe\jump-desktop-`)+32 {
		return errors.New("helper_start_failed")
	}
	parent, e := strconv.ParseUint(args[1], 10, 32)
	if e != nil {
		return e
	}
	session, e := strconv.ParseUint(args[2], 10, 32)
	if e != nil {
		return e
	}
	var own uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &own) != nil || own != uint32(session) {
		return errors.New("no_interactive_session")
	}
	h, e := windows.CreateFile(windows.StringToUTF16Ptr(args[0]), windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if e != nil {
		return e
	}
	defer windows.CloseHandle(h)
	var server uint32
	if windows.GetNamedPipeServerProcessId(h, &server) != nil || server != uint32(parent) {
		return errors.New("helper_start_failed")
	}
	mode := uint32(windows.PIPE_NOWAIT)
	if windows.SetNamedPipeHandleState(h, &mode, nil, nil) != nil {
		return errors.New("helper_start_failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipe := &desktopPipe{ctx: ctx, handle: h}
	input := make(chan message, 64)
	go func() {
		defer cancel()
		for {
			var in message
			if pipe.receive(&in) != nil {
				return
			}
			select {
			case input <- in:
			case <-ctx.Done():
				return
			}
		}
	}()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	trace, closeTrace := newScreenRuntimeTrace()
	defer closeTrace()
	inject := func(in screenInput) bool { return injectScreenInput(in, trace) }
	keys := make(map[int]bool)
	buttons := make(map[int]bool)
	releasedKeys, releasedButtons := make(map[int]bool), make(map[int]bool)
	release := func() {
		for _, in := range releaseHeldScreenInput(keys, buttons) {
			if in.Action == "key" {
				releasedKeys[in.Key] = true
			} else {
				releasedButtons[in.Button] = true
			}
		}
		retryScreenReleases(releasedKeys, releasedButtons, inject)
	}

	defer release()
	original, _, _ := screenUser32.NewProc("GetThreadDesktop").Call(uintptr(windows.GetCurrentThreadId()))
	var follower desktopFollower
	closeDesktop := func(h uintptr) { screenUser32.NewProc("CloseDesktop").Call(h) }
	attach := func(h uintptr) bool {
		r, _, e := screenUser32.NewProc("SetThreadDesktop").Call(h)
		if r != 0 {
			delete(trace.last, "desktop_attach")
		}
		trace.record("desktop_attach", "", r != 0, screenWin32Code(e), "")
		if r != 0 {
			active, _, e := screenUser32.NewProc("GetThreadDesktop").Call(uintptr(windows.GetCurrentThreadId()))
			trace.record("desktop_thread", "", active == h, screenWin32Code(e), "")
		}
		return r != 0
	}
	defer func() {
		release()
		if follower.handle != 0 && attach(original) {
			closeDesktop(follower.handle)
		}
	}()
	var gap, releaseGap time.Time
	follow := func() (bool, bool, error) {
		h, name := openScreenDesktop(trace)
		if h == 0 {
			release()
			if gap.IsZero() {
				gap = time.Now()
			}
			if time.Since(gap) > 5*time.Second {
				return false, false, errors.New("desktop_open_failed")
			}
			return false, false, nil
		}
		initial := follower.handle == 0
		// Keep held-key state until after attaching, then send releases on the new
		// physical input desktop too. Windows may reject releases on the old one.
		oldKeys, oldButtons := make(map[int]bool), make(map[int]bool)
		for k := range keys {
			oldKeys[k] = true
		}
		for b := range buttons {
			oldButtons[b] = true
		}
		oldReceivingInput := false
		if follower.handle != 0 {
			var active int32
			var size uint32
			r, _, e := screenUser32.NewProc("GetUserObjectInformationW").Call(follower.handle, 6, uintptr(unsafe.Pointer(&active)), 4, uintptr(unsafe.Pointer(&size)))
			queried := r != 0 && size >= 4
			trace.record("desktop_old_input", follower.name, queried, screenWin32Code(e), "")
			oldReceivingInput = desktopAttachmentReliable(queried, active != 0)
			trace.record("desktop_old_active", follower.name, oldReceivingInput, 0, "")
		}
		changed, err := follower.follow(h, name, oldReceivingInput, release, attach, closeDesktop)
		if err != nil {
			if gap.IsZero() {
				gap = time.Now()
			}
			if time.Since(gap) > 5*time.Second {
				return false, false, err
			}
			return false, false, nil
		}
		gap = time.Time{}
		for k := range releasedKeys {
			oldKeys[k] = true
		}
		for b := range releasedButtons {
			oldButtons[b] = true
		}
		if changed || len(releasedKeys) > 0 || len(releasedButtons) > 0 {
			for k := range oldKeys {
				releasedKeys[k] = true
			}
			for b := range oldButtons {
				releasedButtons[b] = true
			}
			release() // Failed releases stay pending until an attached desktop accepts them.
		}
		if changed {
			trace.resetCapture()
			trace.record("desktop_changed", name, true, 0, "")
			stage := "desktop_changed"
			if initial {
				stage = "desktop_attached"
			}
			_ = pipe.send(helperFrame{Operation: &message{Type: "screen_event", Stage: stage, Desktop: safeDesktopName(name)}})
		}
		if len(releasedKeys) > 0 || len(releasedButtons) > 0 {
			if releaseGap.IsZero() {
				releaseGap = time.Now()
			}
			if time.Since(releaseGap) > 5*time.Second {
				return false, changed, errors.New("input_failed")
			}
		} else {
			releaseGap = time.Time{}
		}
		return true, changed, nil
	}
	var clipboardOp screenOperation
	clipboardJobs := make(chan message, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-clipboardJobs:
				text, err := windowsClipboard(job.Kind, job.Data)
				code := "ok"
				if err != nil {
					code = err.Error()
				}
				if code == "ok" && job.Kind == "clipboard_get" {
					for _, chunk := range clipboardChunks(job.RequestID, []byte(text)) {
						if pipe.send(helperFrame{Operation: &chunk}) != nil {
							return
						}
					}
				}
				if pipe.send(helperFrame{Operation: &message{Type: "screen_operation_result", RequestID: job.RequestID, Kind: job.Kind, Code: code}}) != nil {
					return
				}
			}
		}
	}()
	ticker := time.NewTicker(time.Second / 6)
	defer ticker.Stop()
	var captureGap time.Time
	var last [32]byte
	first := true
	fail := func(code string) error { _ = pipe.send(helperFrame{Code: code}); return errors.New(code) }
	for {
		select {
		case <-ctx.Done():
			return nil
		case command := <-input:
			if windows.WTSGetActiveConsoleSessionId() != uint32(session) {
				return fail("interactive_session_changed")
			}
			if command.Type != "screen_input" {
				if clipboardOp.request(command) != nil {
					return fail("invalid_frame")
				}
				if command.Type == "screen_operation_cancel" {
					continue
				}
				ready := command.Type == "screen_operation" && command.Kind == "clipboard_get"
				if command.Type == "screen_clipboard" {
					if clipboardOp.response(message{Type: "screen_clipboard_ack", RequestID: command.RequestID, Index: command.Index}) != nil {
						return fail("invalid_frame")
					}
					ready = clipboardOp.index == clipboardOp.count
				}
				if ready {
					job := message{RequestID: clipboardOp.id, Kind: clipboardOp.kind, Data: string(clipboardOp.data)}
					select {
					case clipboardJobs <- job:
					default:
						_ = pipe.send(helperFrame{Operation: &message{Type: "screen_operation_result", RequestID: job.RequestID, Kind: job.Kind, Code: "operation_busy"}})
					}
					clipboardOp.clear()
				}
				continue
			}
			if !validScreenInput(command.Input) {
				return fail("invalid_frame")
			}
			in := *command.Input
			ready, changed, err := follow()
			if err != nil {
				return fail(screenFailure(err))
			}
			if changed {
				first = true
			}
			if !ready {
				continue
			}
			if changed && in.Action != "release" && !screenInputSafeAfterDesktopChange(in) {
				continue
			} // Never replay an old-desktop press or wheel.
			if in.Action == "release" {
				release()
				if len(releasedKeys) > 0 || len(releasedButtons) > 0 {
					ready, changed, err := follow()
					if err != nil {
						return fail(screenFailure(err))
					}
					if changed {
						first = true
					}
					if !ready || changed && (len(releasedKeys) > 0 || len(releasedButtons) > 0) {
						continue
					}
					if len(releasedKeys) > 0 || len(releasedButtons) > 0 {
						return fail("input_failed")
					}
				}
				if pipe.send(helperFrame{Released: true}) != nil {
					return errors.New("input_failed")
				}
				continue
			}
			delivered, changed, err := deliverDesktopInput(in, inject, follow, release)
			if changed {
				first = true
			}
			if err != nil {
				return fail(screenFailure(err))
			}
			if !delivered {
				continue
			}
			if in.Action == "key" {
				if in.Down {
					keys[in.Key] = true
				} else {
					delete(keys, in.Key)
				}
			}
			if in.Action == "button" {
				if in.Down {
					buttons[in.Button] = true
				} else {
					delete(buttons, in.Button)
				}
			}
		case <-ticker.C:
			if windows.WTSGetActiveConsoleSessionId() != uint32(session) {
				return fail("interactive_session_changed")
			}
			ready, changed, err := follow()
			if err != nil {
				return fail(screenFailure(err))
			}
			if changed {
				first = true
			}
			if !ready {
				continue
			}
			if first {
				trace.record("capture_first_attempt", follower.name, true, 0, "")
			}
			f, e := capturePrimary()
			if first {
				code := ""
				if e != nil {
					code = screenFailure(e)
				}
				trace.record("capture_first_result", follower.name, e == nil, 0, code)
			}
			if e != nil {
				ready, changed, err := follow()
				if err != nil {
					return fail(screenFailure(err))
				}
				if !ready || changed {
					first = true
					continue
				}
				if captureGap.IsZero() {
					captureGap = time.Now()
				}
				if time.Since(captureGap) > 5*time.Second {
					return fail(screenFailure(e))
				}
				continue
			}
			captureGap = time.Time{}
			digest := sha256.Sum256(f.JPEG)
			if !first && digest == last {
				continue
			}
			last = digest
			e = pipe.send(helperFrame{Data: f.JPEG, Width: f.Width, Height: f.Height})
			if first {
				trace.record("frame_first_sent", follower.name, e == nil, 0, "")
			}
			if e != nil {
				return e
			}
			first = false
		}
	}
}

type bitmapInfo struct {
	Size         uint32
	Width        int32
	Height       int32
	Planes       uint16
	BitCount     uint16
	Compression  uint32
	SizeImage    uint32
	XPels        int32
	YPels        int32
	ClrUsed      uint32
	ClrImportant uint32
}
type cursorInfo struct {
	Size   uint32
	Flags  uint32
	Cursor windows.Handle
	X      int32
	Y      int32
}
type iconInfo struct {
	Icon     int32
	XHotspot uint32
	YHotspot uint32
	Mask     windows.Handle
	Color    windows.Handle
}

func capturePrimary() (desktopFrame, error) {
	metric := screenUser32.NewProc("GetSystemMetrics")
	w, _, _ := metric.Call(0)
	h, _, _ := metric.Call(1)
	width, height, err := captureDimensions(int(w), int(h))
	if err != nil {
		return desktopFrame{}, err
	}
	var resources screenCaptureResources
	defer resources.close(
		func(dc, bitmap uintptr) { screenGDI32.NewProc("SelectObject").Call(dc, bitmap) },
		func(dc uintptr) { screenGDI32.NewProc("DeleteDC").Call(dc) },
		func(bitmap uintptr) { screenGDI32.NewProc("DeleteObject").Call(bitmap) },
		func(source uintptr) { screenUser32.NewProc("ReleaseDC").Call(0, source) },
	)
	resources.source, _, _ = screenUser32.NewProc("GetDC").Call(0)
	if resources.source == 0 {
		return desktopFrame{}, errors.New("capture_get_dc_failed")
	}
	resources.dc, _, _ = screenGDI32.NewProc("CreateCompatibleDC").Call(resources.source)
	if resources.dc == 0 {
		return desktopFrame{}, errors.New("capture_create_dc_failed")
	}
	// Negative height selects top-down rows; BI_RGB/32bpp needs no color table.
	// With no file mapping, DeleteObject owns the DIB storage lifetime.
	info := bitmapInfo{Size: 40, Width: int32(width), Height: -int32(height), Planes: 1, BitCount: 32}
	var bits unsafe.Pointer
	resources.bitmap, _, _ = screenGDI32.NewProc("CreateDIBSection").Call(
		resources.source, uintptr(unsafe.Pointer(&info)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0,
	)
	if resources.bitmap == 0 {
		return desktopFrame{}, errors.New("capture_create_bitmap_failed")
	}
	if bits == nil {
		return desktopFrame{}, errors.New("capture_pixels_failed")
	}
	old, _, _ := screenGDI32.NewProc("SelectObject").Call(resources.dc, resources.bitmap)
	if old == 0 || old == ^uintptr(0) {
		return desktopFrame{}, errors.New("capture_select_bitmap_failed")
	}
	resources.previous = old
	r, _, _ := screenGDI32.NewProc("SetStretchBltMode").Call(resources.dc, 4)
	if r == 0 {
		return desktopFrame{}, errors.New("capture_stretch_mode_failed")
	}
	// HALFTONE requires resetting the brush origin.
	r, _, _ = screenGDI32.NewProc("SetBrushOrgEx").Call(resources.dc, 0, 0, 0)
	if r == 0 {
		return desktopFrame{}, errors.New("capture_stretch_mode_failed")
	}
	if !captureBlit(func(layered bool) bool {
		rop := uintptr(0x00cc0020) // SRCCOPY
		if layered {
			rop |= 0x40000000 // CAPTUREBLT
		}
		var copied uintptr
		if w == uintptr(width) && h == uintptr(height) {
			copied, _, _ = screenGDI32.NewProc("BitBlt").Call(resources.dc, 0, 0, w, h, resources.source, 0, 0, rop)
		} else {
			copied, _, _ = screenGDI32.NewProc("StretchBlt").Call(resources.dc, 0, 0, uintptr(width), uintptr(height), resources.source, 0, 0, w, h, rop)
		}
		return copied != 0
	}) {
		return desktopFrame{}, errors.New("capture_blit_failed")
	}
	drawCaptureCursor(resources.dc, float64(width)/float64(w), float64(height)/float64(h))
	// GDI may batch writes to a DIB section. Flush on this locked capture thread
	// before touching the pixel pointer (CreateDIBSection synchronization rule).
	r, _, _ = screenGDI32.NewProc("GdiFlush").Call()
	if r == 0 {
		return desktopFrame{}, errors.New("capture_flush_failed")
	}
	img, err := captureImage(unsafe.Slice((*byte)(bits), width*height*4), width, height)
	if err != nil {
		return desktopFrame{}, err
	}
	frame, err := encodeScreenFrame(img)
	if err != nil {
		return desktopFrame{}, errors.New("capture_encode_failed")
	}
	return frame, nil
}

// Cursor overlay is best effort: an unavailable cursor must not discard a
// valid desktop image. GetIconInfo bitmaps are caller-owned, never the HCURSOR.
func drawCaptureCursor(dc uintptr, scaleX, scaleY float64) {
	ci := cursorInfo{Size: uint32(unsafe.Sizeof(cursorInfo{}))}
	r, _, _ := screenUser32.NewProc("GetCursorInfo").Call(uintptr(unsafe.Pointer(&ci)))
	if r == 0 || ci.Flags&1 == 0 {
		return
	}
	var ii iconInfo
	r, _, _ = screenUser32.NewProc("GetIconInfo").Call(uintptr(ci.Cursor), uintptr(unsafe.Pointer(&ii)))
	if r == 0 {
		return
	}
	if ii.Mask != 0 {
		defer screenGDI32.NewProc("DeleteObject").Call(uintptr(ii.Mask))
	}
	if ii.Color != 0 {
		defer screenGDI32.NewProc("DeleteObject").Call(uintptr(ii.Color))
	}
	screenUser32.NewProc("DrawIconEx").Call(
		dc, uintptr(int(float64(ci.X-int32(ii.XHotspot))*scaleX)),
		uintptr(int(float64(ci.Y-int32(ii.YHotspot))*scaleY)), uintptr(ci.Cursor),
		uintptr(max(1, int(32*scaleX))), uintptr(max(1, int(32*scaleY))), 0, 0, 3,
	)
}
func injectScreenInput(in screenInput, trace *screenRuntimeTrace) bool {
	if !validScreenInput(&in) {
		return false
	}
	var raw [40]byte // Supported amd64 INPUT ABI.
	if in.Action == "key" {
		binary.LittleEndian.PutUint32(raw[:], 1)
		binary.LittleEndian.PutUint16(raw[8:], uint16(in.Key))
		flags := uint32(0)
		if !in.Down {
			flags = 2
		}
		if in.Key >= 33 && in.Key <= 46 || in.Key == 91 || in.Key == 92 || in.Key == 0xa3 || in.Key == 0xa5 {
			flags |= 1
		}
		binary.LittleEndian.PutUint32(raw[12:], flags)
	} else {
		var flags uint32
		switch in.Action {
		case "move":
			flags = 0x8001
			binary.LittleEndian.PutUint32(raw[8:], uint32(in.X))
			binary.LittleEndian.PutUint32(raw[12:], uint32(in.Y))
		case "button":
			flags = []uint32{2, 0x20, 8}[in.Button]
			if !in.Down {
				flags <<= 1
			}
		case "wheel":
			flags = 0x800
			binary.LittleEndian.PutUint32(raw[16:], uint32(int32(in.Delta)))
		default:
			return false
		}
		binary.LittleEndian.PutUint32(raw[20:], flags)
	}
	r, _, e := screenUser32.NewProc("SendInput").Call(1, uintptr(unsafe.Pointer(&raw[0])), 40)
	trace.record("send_input", "", r == 1, screenWin32Code(e), "")
	return r == 1
}

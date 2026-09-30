//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"image"
	"image/jpeg"
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
		e := windows.WriteFile(p.handle, b, &n, nil)
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
	Data     []byte `json:"data,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Code     string `json:"code,omitempty"`
	Released bool   `json:"released,omitempty"`
}
type windowsDesktop struct {
	pipe    *desktopPipe
	job     windows.Handle
	session uint32
	once    sync.Once
}

func (d *windowsDesktop) Next(context.Context) (desktopFrame, error) {
	for {
		var f helperFrame
		if e := d.pipe.receive(&f); e != nil {
			return desktopFrame{}, e
		}
		if f.Released {
			continue
		}
		if f.Code != "" {
			return desktopFrame{}, errors.New(f.Code)
		}
		if windows.WTSGetActiveConsoleSessionId() != d.session {
			return desktopFrame{}, errors.New("interactive_session_changed")
		}
		return desktopFrame{f.Data, f.Width, f.Height}, nil
	}
}
func (d *windowsDesktop) Input(in screenInput) error {
	if !validScreenInput(&in) || windows.WTSGetActiveConsoleSessionId() != d.session {
		return errors.New("input_failed")
	}
	return d.pipe.send(in)
}
func (d *windowsDesktop) Close() {
	d.once.Do(func() {
		// Capture reader has stopped before Close. Give the helper a bounded chance
		// to release its own injected keys/buttons before enforcing job termination.
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		pipe := &desktopPipe{ctx: ctx, handle: d.pipe.handle}
		if pipe.send(screenInput{Action: "release"}) == nil {
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
func launchDesktop(ctx context.Context) (desktopBridge, error) {
	if !screenSupported() {
		return nil, errors.New("helper_start_failed")
	}
	session, found := discoverDesktopSession(windows.WTSGetActiveConsoleSessionId, func(id uint32) bool {
		var user windows.Token
		if windows.WTSQueryUserToken(id, &user) != nil {
			return false
		}
		user.Close()
		return true
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
	d := &windowsDesktop{pipe: &desktopPipe{ctx: ctx, handle: pipe}, job: job, session: session}
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
	ok = true
	return d, nil
}
func ordinaryDesktop() bool {
	h, _, _ := screenUser32.NewProc("OpenInputDesktop").Call(0, 0, 1)
	if h == 0 {
		return false
	}
	defer screenUser32.NewProc("CloseDesktop").Call(h)
	var name [256]uint16
	var size uint32
	r, _, _ := screenUser32.NewProc("GetUserObjectInformationW").Call(h, 2, uintptr(unsafe.Pointer(&name[0])), 512, uintptr(unsafe.Pointer(&size)))
	return r != 0 && strings.EqualFold(windows.UTF16ToString(name[:]), "Default")
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
	input := make(chan screenInput, 64)
	go func() {
		defer cancel()
		for {
			var in screenInput
			if pipe.receive(&in) != nil || !validScreenInput(&in) {
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
	keys := make(map[int]bool)
	buttons := make(map[int]bool)
	release := func() {
		if !ordinaryDesktop() {
			clear(keys)
			clear(buttons)
			return
		}
		for k := range keys {
			injectScreenInput(screenInput{Action: "key", Key: k})
		}
		for b := range buttons {
			injectScreenInput(screenInput{Action: "button", Button: b})
		}
		clear(keys)
		clear(buttons)
	}
	defer release()
	ticker := time.NewTicker(time.Second / 6)
	defer ticker.Stop()
	var last [32]byte
	first := true
	fail := func(code string) error { _ = pipe.send(helperFrame{Code: code}); return errors.New(code) }
	for {
		select {
		case <-ctx.Done():
			return nil
		case in := <-input:
			if windows.WTSGetActiveConsoleSessionId() != uint32(session) {
				return fail("interactive_session_changed")
			}
			if !ordinaryDesktop() {
				return fail("secure_desktop")
			}
			if in.Action == "release" {
				release()
				if pipe.send(helperFrame{Released: true}) != nil {
					return errors.New("input_failed")
				}
				continue
			}
			if !injectScreenInput(in) {
				return fail("input_failed")
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
			if !ordinaryDesktop() {
				return fail("secure_desktop")
			}
			f, e := capturePrimary()
			if e != nil {
				return fail("capture_failed")
			}
			digest := sha256.Sum256(f.JPEG)
			if !first && digest == last {
				continue
			}
			first = false
			last = digest
			if e = pipe.send(helperFrame{Data: f.JPEG, Width: f.Width, Height: f.Height}); e != nil {
				return e
			}
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
	if w == 0 || h == 0 || w > 16384 || h > 16384 {
		return desktopFrame{}, errors.New("capture_failed")
	}
	scale := 1.0
	if float64(w) > screenMaxWidth {
		scale = float64(screenMaxWidth) / float64(w)
	}
	if float64(h)*scale > screenMaxHeight {
		scale = float64(screenMaxHeight) / float64(h)
	}
	width, height := int(float64(w)*scale), int(float64(h)*scale)
	source, _, _ := screenUser32.NewProc("GetDC").Call(0)
	if source == 0 {
		return desktopFrame{}, errors.New("capture_failed")
	}
	defer screenUser32.NewProc("ReleaseDC").Call(0, source)
	dc, _, _ := screenGDI32.NewProc("CreateCompatibleDC").Call(source)
	if dc == 0 {
		return desktopFrame{}, errors.New("capture_failed")
	}
	defer screenGDI32.NewProc("DeleteDC").Call(dc)
	bitmap, _, _ := screenGDI32.NewProc("CreateCompatibleBitmap").Call(source, uintptr(width), uintptr(height))
	if bitmap == 0 {
		return desktopFrame{}, errors.New("capture_failed")
	}
	defer screenGDI32.NewProc("DeleteObject").Call(bitmap)
	old, _, _ := screenGDI32.NewProc("SelectObject").Call(dc, bitmap)
	screenGDI32.NewProc("SetStretchBltMode").Call(dc, 4)
	r, _, _ := screenGDI32.NewProc("StretchBlt").Call(dc, 0, 0, uintptr(width), uintptr(height), source, 0, 0, w, h, 0x40cc0020)
	if r == 0 {
		screenGDI32.NewProc("SelectObject").Call(dc, old)
		return desktopFrame{}, errors.New("capture_failed")
	}
	ci := cursorInfo{Size: uint32(unsafe.Sizeof(cursorInfo{}))}
	if r, _, _ := screenUser32.NewProc("GetCursorInfo").Call(uintptr(unsafe.Pointer(&ci))); r != 0 && ci.Flags&1 != 0 {
		var ii iconInfo
		if r, _, _ := screenUser32.NewProc("GetIconInfo").Call(uintptr(ci.Cursor), uintptr(unsafe.Pointer(&ii))); r != 0 {
			screenUser32.NewProc("DrawIconEx").Call(dc, uintptr(int(float64(ci.X-int32(ii.XHotspot))*scale)), uintptr(int(float64(ci.Y-int32(ii.YHotspot))*scale)), uintptr(ci.Cursor), uintptr(int(32*scale)), uintptr(int(32*scale)), 0, 0, 3)
			if ii.Mask != 0 {
				screenGDI32.NewProc("DeleteObject").Call(uintptr(ii.Mask))
			}
			if ii.Color != 0 {
				screenGDI32.NewProc("DeleteObject").Call(uintptr(ii.Color))
			}
		}
	}
	screenGDI32.NewProc("SelectObject").Call(dc, old)
	data := make([]byte, width*height*4)
	info := bitmapInfo{Size: 40, Width: int32(width), Height: -int32(height), Planes: 1, BitCount: 32}
	if r, _, _ := screenGDI32.NewProc("GetDIBits").Call(dc, bitmap, 0, uintptr(height), uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&info)), 0); r != uintptr(height) {
		return desktopFrame{}, errors.New("capture_failed")
	}
	for i := 0; i < len(data); i += 4 {
		data[i], data[i+2], data[i+3] = data[i+2], data[i], 255
	}
	img := &image.RGBA{Pix: data, Stride: width * 4, Rect: image.Rect(0, 0, width, height)}
	var encoded bytes.Buffer
	if jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 65}) != nil || encoded.Len() > screenMaxFrame {
		return desktopFrame{}, errors.New("capture_failed")
	}
	return desktopFrame{encoded.Bytes(), width, height}, nil
}
func injectScreenInput(in screenInput) bool {
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
	r, _, _ := screenUser32.NewProc("SendInput").Call(1, uintptr(unsafe.Pointer(&raw[0])), 40)
	return r == 1
}

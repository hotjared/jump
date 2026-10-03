//go:build windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Exercise the real nonblocking byte pipe, including JSON/base64 expansion
// beyond its 64 KiB buffer. A small Winlogon frame can conceal this failure.
func TestDesktopPipeLargeFrame(t *testing.T) {
	for _, size := range []int{32000, 64000, 200000, screenMaxFrame} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			name := windows.StringToUTF16Ptr(fmt.Sprintf(`\\.\pipe\jump-pipe-test-%d-%d`, os.Getpid(), time.Now().UnixNano()))
			server, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE, windows.PIPE_TYPE_BYTE|windows.PIPE_NOWAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 65536, 65536, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(server)
			client, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(client)
			mode := uint32(windows.PIPE_NOWAIT)
			if err := windows.SetNamedPipeHandleState(client, &mode, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := windows.ConnectNamedPipe(server, nil); err != nil && err != windows.ERROR_PIPE_CONNECTED {
				t.Fatal(err)
			}
			writer := &desktopPipe{ctx: ctx, handle: client}
			reader := &desktopPipe{ctx: ctx, handle: server}
			want := helperFrame{Data: bytes.Repeat([]byte{0x5a}, size), Width: 1920, Height: 1080}
			sent := make(chan error, 1)
			go func() { sent <- writer.send(want) }()
			var got helperFrame
			if err := reader.receive(&got); err != nil {
				t.Fatalf("receive: %v; send: %v", err, <-sent)
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Data, want.Data) || got.Width != want.Width || got.Height != want.Height {
				t.Fatal("frame corrupted")
			}
		})
	}
}

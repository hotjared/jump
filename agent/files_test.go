package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

func harness() (*fileMux, chan message) {
	frames := make(chan message, 256)
	return newFileMux(func(msg message) error { frames <- msg; return nil }), frames
}
func response(t *testing.T, frames chan message) message {
	t.Helper()
	select {
	case msg := <-frames:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
		return message{}
	}
}
func TestFileCapability(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		if !strings.Contains(strings.Join(capabilitiesFor(platform, "amd64"), ","), "file_transfer_v1") {
			t.Fatal(platform)
		}
	}
}
func TestCancelledDownloadStopsAfterFirstChunk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux temp paths")
	}
	path := filepath.Join(t.TempDir(), "large.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), fileChunkSize*3), 0600); err != nil {
		t.Fatal(err)
	}
	mux, frames := harness()
	mux.handle(message{Type: "file_download_open", TransferID: testID, Path: path})
	if got := response(t, frames); got.Type != "file_opened" {
		t.Fatal(got)
	}
	if got := response(t, frames); got.Type != "file_chunk" {
		t.Fatal(got)
	}
	mux.handle(message{Type: "file_cancel", TransferID: testID})
	mux.handle(message{Type: "file_cancel", TransferID: testID})
	mux.handle(message{Type: "file_ack", TransferID: testID})
	if len(mux.streams) != 0 {
		t.Fatal("cancelled download route remained active")
	}
	select {
	case got := <-frames:
		t.Fatalf("download emitted after cancellation: %s", got.Type)
	case <-time.After(20 * time.Millisecond):
	}
}
func TestFileRoundTrip(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux temp paths")
	}
	dir := t.TempDir()
	mux, frames := harness()
	defer mux.closeAll()
	payload := bytes.Repeat([]byte("chunk-data"), 2000)
	sum := sha256.Sum256(payload)
	mux.handle(message{Type: "file_upload_open", TransferID: testID, Path: dir, Name: "sample.bin", Size: int64(len(payload))})
	if got := response(t, frames); got.Type != "file_opened" {
		t.Fatal(got)
	}
	names, _ := filepath.Glob(filepath.Join(dir, ".jump-*.tmp"))
	if len(names) != 1 {
		t.Fatal("temporary file missing")
	}
	for offset := 0; offset < len(payload); offset += fileChunkSize {
		end := offset + fileChunkSize
		if end > len(payload) {
			end = len(payload)
		}
		mux.handle(message{Type: "file_chunk", TransferID: testID, Data: base64.StdEncoding.EncodeToString(payload[offset:end])})
		if got := response(t, frames); got.Size != int64(end) {
			t.Fatal(got)
		}
	}
	mux.handle(message{Type: "file_finish", TransferID: testID})
	if got := response(t, frames); got.Type != "file_finished" || got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal(got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sample.bin"))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatal(err)
	}
	mux.handle(message{Type: "file_upload_open", TransferID: testID, Path: dir, Name: "sample.bin", Size: 3})
	if got := response(t, frames); got.Code != "destination_exists" {
		t.Fatal(got)
	}
	mux.handle(message{Type: "file_upload_open", TransferID: testID, Path: dir, Name: "sample.bin", Size: 3, Overwrite: true})
	response(t, frames)
	mux.handle(message{Type: "file_chunk", TransferID: testID, Data: base64.StdEncoding.EncodeToString([]byte("new"))})
	response(t, frames)
	mux.handle(message{Type: "file_finish", TransferID: testID})
	response(t, frames)
	data, _ = os.ReadFile(filepath.Join(dir, "sample.bin"))
	if string(data) != "new" {
		t.Fatal("overwrite failed")
	}
	mux.handle(message{Type: "file_upload_open", TransferID: testID, Path: dir, Name: "cancel.bin", Size: 100})
	response(t, frames)
	mux.handle(message{Type: "file_cancel", TransferID: testID})
	names, _ = filepath.Glob(filepath.Join(dir, ".jump-*.tmp"))
	if len(names) != 0 {
		t.Fatal("temporary file remained")
	}
	mux.handle(message{Type: "file_download_open", TransferID: testID, Path: filepath.Join(dir, "sample.bin")})
	var downloaded []byte
	for {
		got := response(t, frames)
		if got.Type == "file_opened" {
			continue
		}
		if got.Type == "file_chunk" {
			b, _ := base64.StdEncoding.DecodeString(got.Data)
			downloaded = append(downloaded, b...)
			mux.handle(message{Type: "file_ack", TransferID: testID})
			continue
		}
		if got.Type != "file_finished" || string(downloaded) != "new" {
			t.Fatal(got)
		}
		break
	}
}
func TestFilePaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux temp paths")
	}
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	os.Mkdir(empty, 0700)
	os.WriteFile(filepath.Join(dir, "regular"), []byte("ok"), 0600)
	os.Symlink("/etc", filepath.Join(dir, "link"))
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	mux, frames := harness()
	mux.handle(message{Type: "file_list", TransferID: testID, Path: dir})
	if got := response(t, frames); got.Type != "file_list_result" || len(got.Entries) != 2 {
		t.Fatal(got)
	}
	mux.handle(message{Type: "file_list", TransferID: testID, Path: empty})
	if got := response(t, frames); got.Type != "file_list_result" || got.Entries == nil || len(got.Entries) != 0 {
		t.Fatal(got)
	}
	mux.handle(message{Type: "file_download_open", TransferID: testID, Path: filepath.Join(dir, "link", "passwd")})
	if got := response(t, frames); got.Type != "file_error" {
		t.Fatal(got)
	}
	mux.handle(message{Type: "file_download_open", TransferID: testID, Path: filepath.Join(dir, "pipe")})
	if got := response(t, frames); got.Code != "not_a_regular_file" {
		t.Fatal(got)
	}
	mux.handle(message{Type: "file_download_open", TransferID: testID, Path: dir + "/../etc/passwd"})
	if got := response(t, frames); got.Code != "invalid_path" {
		t.Fatal(got)
	}
	for _, path := range []string{`\\server\share`, `\\?\C:\secret`, `C:relative`, `C:\..\evil`} {
		if root, _, err := fileRoot(path); err == nil {
			root.Close()
			t.Fatal(path)
		}
	}
}

func TestFileConcurrencyAndTransferIsolation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux temp paths")
	}
	dir := t.TempDir()
	mux, frames := harness()
	defer mux.closeAll()
	ids := []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "33333333-3333-3333-3333-333333333333", "44444444-4444-4444-4444-444444444444", "55555555-5555-5555-5555-555555555555"}
	for i, id := range ids {
		mux.handle(message{Type: "file_upload_open", TransferID: id, Path: dir, Name: fmt.Sprintf("%d.bin", i), Size: 1})
		got := response(t, frames)
		if i < 4 && got.Type != "file_opened" {
			t.Fatal(got)
		}
		if i == 4 && got.Type != "file_error" {
			t.Fatal("concurrency bound", got)
		}
	}
	mux.handle(message{Type: "file_chunk", TransferID: ids[1], Data: base64.StdEncoding.EncodeToString([]byte("B"))})
	if got := response(t, frames); got.TransferID != ids[1] || got.Size != 1 {
		t.Fatal(got)
	}
	mux.handle(message{Type: "file_chunk", TransferID: ids[0], Data: base64.StdEncoding.EncodeToString([]byte("A"))})
	if got := response(t, frames); got.TransferID != ids[0] || got.Size != 1 {
		t.Fatal(got)
	}
	mux.handle(message{Type: "file_finish", TransferID: ids[0]})
	response(t, frames)
	mux.handle(message{Type: "file_finish", TransferID: ids[1]})
	response(t, frames)
	first, _ := os.ReadFile(filepath.Join(dir, "0.bin"))
	second, _ := os.ReadFile(filepath.Join(dir, "1.bin"))
	if string(first) != "A" || string(second) != "B" {
		t.Fatal("cross-stream bytes")
	}
	mux.closeAll()
	names, _ := filepath.Glob(filepath.Join(dir, ".jump-*.tmp"))
	if len(names) != 0 {
		t.Fatal("disconnect left temporary files")
	}
}

func TestWindowsNamespaceParsing(t *testing.T) {
	for _, path := range []string{`\\server\share`, `\\?\C:\secret`, `\\.\PIPE\test`, `C:relative`, `C:\..\evil`, `C:\a:stream`, `C:\folder\\file`, `C:/folder`, `C:\trailing.`, `C:\trailing `} {
		if _, _, ok := windowsLocalPath(path); ok {
			t.Fatalf("unsafe Windows path accepted: %q", path)
		}
	}
	drive, rel, ok := windowsLocalPath(`c:\Users\Public`)
	if !ok || drive != `C:\` || rel != `Users\Public` {
		t.Fatal(drive, rel, ok)
	}
}

func TestPosixNamesDoNotUseWindowsRules(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX filenames")
	}
	dir := t.TempDir()
	mux, frames := harness()
	defer mux.closeAll()
	names := []string{"report:2026.txt", "name.", "name ", `back\slash`}
	for i, name := range names {
		if !validPosixPart(name) || validWindowsPart(name) {
			t.Fatalf("validation mismatch: %q", name)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("listed"), 0600); err != nil {
			t.Fatal(err)
		}
		mux.handle(message{Type: "file_download_open", TransferID: fmt.Sprintf("%08d-1111-1111-1111-111111111111", i), Path: path})
		if got := response(t, frames); got.Type != "file_opened" {
			t.Fatal(got)
		}
		if got := response(t, frames); got.Type != "file_chunk" {
			t.Fatal(got)
		} else {
			mux.handle(message{Type: "file_ack", TransferID: got.TransferID})
		}
		if got := response(t, frames); got.Type != "file_finished" {
			t.Fatal(got)
		}
	}
	mux.handle(message{Type: "file_list", TransferID: testID, Path: dir})
	listed := response(t, frames)
	if listed.Type != "file_list_result" || len(listed.Entries) != len(names) {
		t.Fatal(listed)
	}
	for i, name := range names {
		id := fmt.Sprintf("%08d-2222-2222-2222-222222222222", i)
		target := "copy-" + name
		mux.handle(message{Type: "file_upload_open", TransferID: id, Path: dir, Name: target, Size: 1})
		if got := response(t, frames); got.Type != "file_opened" {
			t.Fatal(got)
		}
		mux.handle(message{Type: "file_chunk", TransferID: id, Data: base64.StdEncoding.EncodeToString([]byte("X"))})
		response(t, frames)
		mux.handle(message{Type: "file_finish", TransferID: id})
		if got := response(t, frames); got.Type != "file_finished" {
			t.Fatal(got)
		}
		content, err := os.ReadFile(filepath.Join(dir, target))
		if err != nil || string(content) != "X" {
			t.Fatal(name, err)
		}
	}
}

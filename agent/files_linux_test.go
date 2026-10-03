//go:build linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestFilePaths(t *testing.T) {
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

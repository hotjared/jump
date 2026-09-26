package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type responseTransport struct{ body string }

func (t responseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(t.body)), ContentLength: int64(len(t.body)), Header: http.Header{}}, nil
}

func TestUpdateValidationAndChecksum(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("unsupported platform")
	}
	oldVersion := version
	version = "v0.1.2"
	defer func() { version = oldVersion }()
	content := "official jump agent bytes"
	sum := sha256.Sum256([]byte(content))
	msg := message{Version: 1, Type: "agent_update", OperationID: "11111111-1111-1111-1111-111111111111",
		TargetVersion: "v0.1.3", Platform: runtime.GOOS, Architecture: runtime.GOARCH,
		DownloadURL: "https://github.com/hotjared/jump/releases/download/v0.1.3/" + updateAsset(runtime.GOOS),
		SHA256:      hex.EncodeToString(sum[:])}
	if err := validateUpdate(msg); err != nil {
		t.Fatal(err)
	}
	bad := msg
	bad.TargetVersion = "v0.1.1"
	if validateUpdate(bad) == nil {
		t.Fatal("downgrade accepted")
	}
	bad = msg
	bad.Platform = "other"
	if validateUpdate(bad) == nil {
		t.Fatal("wrong platform accepted")
	}
	bad = msg
	bad.Architecture = "arm64"
	if validateUpdate(bad) == nil {
		t.Fatal("wrong architecture accepted")
	}
	bad = msg
	bad.SHA256 = "bad"
	if validateUpdate(bad) == nil {
		t.Fatal("bad checksum accepted")
	}
	bad = msg
	bad.DownloadURL = "https://example.com/jump-agent"
	if validateUpdate(bad) == nil {
		t.Fatal("untrusted URL accepted")
	}
	bad = msg
	bad.DownloadURL += "?unexpected=1"
	if validateUpdate(bad) == nil {
		t.Fatal("URL query accepted")
	}
	previous := updateHTTPClient
	updateHTTPClient = func() *http.Client { return &http.Client{Transport: responseTransport{content}} }
	defer func() { updateHTTPClient = previous }()
	path := filepath.Join(t.TempDir(), "agent.tmp")
	if err := downloadUpdate(context.Background(), msg, path); err != nil {
		t.Fatal(err)
	}
	if err := verifyStaged(path, msg.SHA256); err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	bad = msg
	bad.SHA256 = strings.Repeat("0", 64)
	if err := downloadUpdate(context.Background(), bad, path); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed download left file: %v", err)
	}
}

func TestUpdateCapability(t *testing.T) {
	has := false
	for _, capability := range info().Capabilities {
		if capability == "agent_update_v1" {
			has = true
		}
	}
	expected := runtime.GOARCH == "amd64" && (runtime.GOOS == "linux" || runtime.GOOS == "windows")
	if has != expected {
		t.Fatalf("capability %v, expected %v", has, expected)
	}
}

func TestReleaseRedirectsAreRestricted(t *testing.T) {
	client := releaseHTTPClient()
	for _, raw := range []string{"http://release-assets.githubusercontent.com/file", "https://evil.example/file", "file:///tmp/agent"} {
		u, _ := url.Parse(raw)
		if client.CheckRedirect(&http.Request{URL: u}, []*http.Request{{}}) == nil {
			t.Fatalf("accepted redirect %s", raw)
		}
	}
	u, _ := url.Parse("https://release-assets.githubusercontent.com/file")
	if err := client.CheckRedirect(&http.Request{URL: u}, []*http.Request{{}}); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsStyleReplacementAndRollback(t *testing.T) {
	for _, failStart := range []bool{false, true} {
		directory := t.TempDir()
		target := filepath.Join(directory, "jump-agent.exe")
		staged := target + ".update"
		backup := target + ".backup"
		if err := os.WriteFile(target, []byte("old"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(staged, []byte("new"), 0700); err != nil {
			t.Fatal(err)
		}
		starts, stops := 0, 0
		err := replaceStoppedBinary(target, staged, backup,
			func() error {
				starts++
				if failStart && starts == 1 {
					return os.ErrPermission
				}
				return nil
			},
			func() bool { return true },
			func() error { stops++; return nil })
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if failStart && (string(data) != "old" || starts != 2 || stops != 1) {
			t.Fatalf("rollback failed: %s, %d, %d", data, starts, stops)
		}
		if !failStart && (string(data) != "new" || starts != 1 || stops != 0) {
			t.Fatalf("replace failed: %s, %d, %d", data, starts, stops)
		}
	}
}

package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var errUpdateHandoff = errors.New("agent update handed off")

func releaseParts(value string) ([3]uint64, bool) {
	var result [3]uint64
	if !strings.HasPrefix(value, "v") {
		return result, false
	}
	parts := strings.Split(value[1:], ".")
	if len(parts) != 3 {
		return result, false
	}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return result, false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return result, false
			}
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return result, false
		}
		result[i] = n
	}
	return result, true
}

func newer(current, target string) bool {
	a, validA := releaseParts(current)
	b, validB := releaseParts(target)
	if !validA || !validB {
		return false
	}
	for i := 0; i < 3; i++ {
		if b[i] > a[i] {
			return true
		}
		if b[i] < a[i] {
			return false
		}
	}
	return false
}

func updateAsset(platform string) string {
	if platform == "linux" {
		return "jump-agent-linux-amd64"
	}
	if platform == "windows" {
		return "jump-agent-windows-amd64.exe"
	}
	return ""
}

func validateUpdate(msg message) error {
	if msg.Version != 1 || msg.Type != "agent_update" || !validOperation(msg.OperationID) ||
		msg.Platform != runtime.GOOS || msg.Architecture != runtime.GOARCH ||
		updateAsset(msg.Platform) == "" || !newer(version, msg.TargetVersion) {
		return errors.New("unsupported update")
	}
	if len(msg.SHA256) != 64 {
		return errors.New("invalid checksum")
	}
	if _, err := hex.DecodeString(msg.SHA256); err != nil {
		return errors.New("invalid checksum")
	}
	u, err := url.Parse(msg.DownloadURL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.Path != "/hotjared/jump/releases/download/"+msg.TargetVersion+"/"+updateAsset(msg.Platform) {
		return errors.New("untrusted release URL")
	}
	return nil
}

func validOperation(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func releaseHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 2 || req.URL.Scheme != "https" || req.URL.Host != "release-assets.githubusercontent.com" {
			return errors.New("untrusted release redirect")
		}
		return nil
	}}
}

var updateHTTPClient = releaseHTTPClient

func downloadUpdate(ctx context.Context, msg message, destination string) error {
	if err := validateUpdate(msg); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", msg.DownloadURL, nil)
	if err != nil {
		return err
	}
	response, err := updateHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.ContentLength > 100*1024*1024 {
		return errors.New("release download failed")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			os.Remove(destination)
		}
	}()
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, 100*1024*1024+1))
	if err != nil {
		return err
	}
	if n == 0 || n > 100*1024*1024 {
		return errors.New("invalid release size")
	}
	expected, _ := hex.DecodeString(msg.SHA256)
	if subtle.ConstantTimeCompare(hash.Sum(nil), expected) != 1 {
		return errors.New("checksum mismatch")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Chmod(0755); err != nil {
		return err
	}
	complete = true
	return nil
}

func updateStatus(mux *sshMux, msg message, state, reason string) {
	_ = mux.send(message{Version: 1, Type: "agent_update_status", OperationID: msg.OperationID, State: state, Reason: reason})
}

func performUpdate(mux *sshMux, msg message) error {
	if err := validateUpdate(msg); err != nil {
		return err
	}
	target := installedAgentPath()
	self, err := os.Executable()
	if err != nil || !strings.EqualFold(filepath.Clean(self), filepath.Clean(target)) {
		updateStatus(mux, msg, "failed", "install_failed")
		return errors.New("agent must run as the installed service")
	}
	staged := target + ".update-" + msg.OperationID
	updateStatus(mux, msg, "downloading", "")
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	if err := downloadUpdate(ctx, msg, staged); err != nil {
		reason := "download_failed"
		if strings.Contains(err.Error(), "checksum mismatch") {
			reason = "checksum_mismatch"
		}
		updateStatus(mux, msg, "failed", reason)
		return err
	}
	updateStatus(mux, msg, "installing", "")
	updateStatus(mux, msg, "restarting", "")
	if err := launchUpdateHelper(msg.OperationID, msg.SHA256); err != nil {
		os.Remove(staged)
		updateStatus(mux, msg, "failed", "install_failed")
		return fmt.Errorf("launch update helper: %w", err)
	}
	return nil
}

func verifyStaged(path, expected string) error {
	if len(expected) != 64 {
		return errors.New("invalid checksum")
	}
	want, err := hex.DecodeString(expected)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 100*1024*1024+1)); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(want, hash.Sum(nil)) != 1 {
		return errors.New("checksum mismatch")
	}
	return nil
}

// replaceStoppedBinary is used by the Windows helper after the service exits.
// Keeping the old executable as a separate file makes rollback possible even
// when Windows refuses to overwrite an open executable.
func replaceStoppedBinary(target, staged, backup string, start func() error, ready func() bool, stop func() error) error {
	if err := os.Rename(target, backup); err != nil {
		_ = start()
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(backup, target)
		_ = start()
		return err
	}
	if err := start(); err == nil && ready() {
		return os.Remove(backup)
	}
	if err := stop(); err != nil {
		return err
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	if err := os.Rename(backup, target); err != nil {
		return err
	}
	return start()
}

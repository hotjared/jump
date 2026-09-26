package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// version is set from the release tag with -ldflags; source builds identify as dev.
var version = "dev"

type metadata struct {
	Hostname     string   `json:"hostname"`
	OSFamily     string   `json:"os_family"`
	OSVersion    string   `json:"os_version"`
	Architecture string   `json:"architecture"`
	AgentVersion string   `json:"agent_version"`
	Capabilities []string `json:"capabilities"`
	Addresses    []string `json:"addresses"`
	CurrentUser  string   `json:"current_user,omitempty"`
}
type identity struct {
	Server     string `json:"server"`
	DeviceID   string `json:"device_id"`
	PrivateKey string `json:"private_key"`
}
type message struct {
	Version       int      `json:"version"`
	Type          string   `json:"type"`
	DeviceID      string   `json:"device_id,omitempty"`
	ConnectionID  string   `json:"connection_id,omitempty"`
	Challenge     string   `json:"challenge,omitempty"`
	Signature     string   `json:"signature,omitempty"`
	Metadata      metadata `json:"metadata,omitempty"`
	SessionID     string   `json:"session_id,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	Username      string   `json:"username,omitempty"`
	Secret        string   `json:"secret,omitempty"`
	HostKey       string   `json:"host_key,omitempty"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	Code          string   `json:"code,omitempty"`
	Data          string   `json:"data,omitempty"`
	Columns       int      `json:"columns,omitempty"`
	Rows          int      `json:"rows,omitempty"`
	OperationID   string   `json:"operation_id,omitempty"`
	TargetVersion string   `json:"target_version,omitempty"`
	Platform      string   `json:"platform,omitempty"`
	Architecture  string   `json:"architecture,omitempty"`
	DownloadURL   string   `json:"download_url,omitempty"`
	SHA256        string   `json:"sha256,omitempty"`
	State         string   `json:"state,omitempty"`
	Reason        string   `json:"reason,omitempty"`
}

func info() metadata {
	host, _ := os.Hostname()
	current, _ := user.Current()
	addresses := []string{}
	if interfaces, err := net.Interfaces(); err == nil {
		for _, iface := range interfaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			ips, _ := iface.Addrs()
			for _, address := range ips {
				ip, _, err := net.ParseCIDR(address.String())
				if err == nil && ip.IsGlobalUnicast() && len(addresses) < 16 {
					addresses = append(addresses, ip.String())
				}
			}
		}
	}
	caps := []string{"filesystem", "system_info"}
	if runtime.GOARCH == "amd64" && (runtime.GOOS == "linux" || runtime.GOOS == "windows") {
		caps = append(caps, "agent_update_v1")
	}
	if runtime.GOOS == "windows" {
		caps = append(caps, "rdp", "powershell")
	} else {
		caps = append(caps, "ssh", "shell")
		if runtime.GOOS == "linux" {
			caps = append(caps, "ssh_terminal_v1")
		}
	}
	username := ""
	if current != nil {
		username = current.Username
	}
	osVersion := runtime.GOOS
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					osVersion = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), "\"")
					break
				}
			}
		}
	} else if runtime.GOOS == "windows" {
		if output, err := exec.Command("cmd", "/c", "ver").Output(); err == nil {
			osVersion = strings.TrimSpace(string(output))
		}
	}
	return metadata{host, runtime.GOOS, osVersion, runtime.GOARCH, version, caps, addresses, username}
}

func statePath() string {
	if p := os.Getenv("JUMP_AGENT_STATE"); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramData"), "Jump", "identity.json")
	}
	return "/var/lib/jump-agent/identity.json"
}

func save(path string, value identity) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// Remove inherited access before the private key file is created.
		err := exec.Command("icacls", filepath.Dir(path), "/inheritance:r", "/grant:r",
			"*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F").Run()
		if err != nil {
			return fmt.Errorf("failed to restrict identity directory ACL: %w", err)
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func validateServer(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("invalid server URL")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1") {
		return nil
	}
	return errors.New("server must use HTTPS except for loopback development")
}

func enroll(server, token string) error {
	if err := validateServer(server); err != nil {
		return err
	}
	if _, err := os.Stat(statePath()); err == nil {
		return errors.New("identity already exists")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	request := map[string]any{"token": token, "public_key": base64.StdEncoding.EncodeToString(public), "metadata": info()}
	body, _ := json.Marshal(request)
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Post(strings.TrimRight(server, "/")+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("enrollment rejected: HTTP %d", response.StatusCode)
	}
	var result struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result); err != nil {
		return err
	}
	if len(result.DeviceID) != 36 {
		return errors.New("invalid server response")
	}
	if err := save(statePath(), identity{server, result.DeviceID, base64.StdEncoding.EncodeToString(private)}); err != nil {
		return fmt.Errorf("enrollment succeeded but saving identity failed; revoke this device and enroll again: %w", err)
	}
	slog.Info("enrolled", "device_id", result.DeviceID)
	return nil
}

func connect(ctx context.Context, id identity) error {
	private, err := base64.StdEncoding.DecodeString(id.PrivateKey)
	if err != nil || len(private) != ed25519.PrivateKeySize {
		return errors.New("invalid stored identity")
	}
	endpoint := strings.TrimRight(id.Server, "/") + "/connect?device_id=" + url.QueryEscape(id.DeviceID)
	if strings.HasPrefix(endpoint, "https://") {
		endpoint = "wss://" + strings.TrimPrefix(endpoint, "https://")
	}
	if strings.HasPrefix(endpoint, "http://") {
		endpoint = "ws://" + strings.TrimPrefix(endpoint, "http://")
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(64 * 1024)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var challenge message
	if err := conn.ReadJSON(&challenge); err != nil {
		return err
	}
	nonce, err := base64.StdEncoding.DecodeString(challenge.Challenge)
	if err != nil || challenge.Version != 1 || challenge.Type != "challenge" || len(nonce) != 32 {
		return errors.New("bad challenge")
	}
	signature := ed25519.Sign(private, append([]byte("jump-agent-v1:"), nonce...))
	if err := conn.WriteJSON(message{Version: 1, Type: "auth", DeviceID: id.DeviceID,
		Signature: base64.StdEncoding.EncodeToString(signature), Metadata: info()}); err != nil {
		return err
	}
	var ready message
	if err := conn.ReadJSON(&ready); err != nil {
		return err
	}
	if ready.Version != 1 || ready.Type != "ready" {
		return errors.New("authentication rejected")
	}
	slog.Info("connected", "device_id", id.DeviceID)
	mux := &sshMux{conn: conn, streams: make(map[string]*sshStream)}
	defer mux.closeAll()
	conn.SetReadDeadline(time.Now().Add(75 * time.Second))
	conn.SetPingHandler(func(data string) error {
		conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	done := make(chan error, 1)
	updating := make(chan error, 1)
	var updateStarted atomic.Bool
	go func() {
		for {
			var incoming message
			if err := conn.ReadJSON(&incoming); err != nil {
				done <- err
				return
			}
			if incoming.Version == 1 && incoming.Type == "agent_update" {
				if updateStarted.CompareAndSwap(false, true) {
					go func() {
						err := performUpdate(mux, incoming)
						if err != nil {
							updateStarted.Store(false)
						}
						updating <- err
					}()
				}
				continue
			}
			if incoming.Version != 1 || !mux.handle(incoming) {
				done <- errors.New("invalid session frame")
				return
			}
		}
	}()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, "shutdown"), time.Now().Add(time.Second))
			return nil
		case err := <-done:
			return err
		case err := <-updating:
			if err == nil {
				return errUpdateHandoff
			}
			slog.Warn("agent update failed", "error", err)
		case <-ticker.C:
			if err := mux.send(message{Version: 1, Type: "heartbeat"}); err != nil {
				return err
			}
		}
	}
}

func run(ctx context.Context) error {
	data, err := os.ReadFile(statePath())
	if err != nil {
		return err
	}
	var id identity
	if err := json.Unmarshal(data, &id); err != nil {
		return err
	}
	if err := validateServer(id.Server); err != nil {
		return err
	}
	attempt := 0
	for ctx.Err() == nil {
		if err := connect(ctx, id); err != nil {
			if errors.Is(err, errUpdateHandoff) {
				return err
			}
			slog.Warn("connection lost", "device_id", id.DeviceID, "error", err)
		}
		duration := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt)), float64(time.Minute)))
		delay := duration/2 + time.Duration(mathrand.Int63n(int64(duration/2)))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if attempt < 6 {
			attempt++
		}
	}
	return nil
}

func runForeground() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: jump-agent enroll --server URL --token TOKEN | run | service install|uninstall|start|stop")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "enroll":
		flags := flag.NewFlagSet("enroll", flag.ExitOnError)
		server := flags.String("server", "", "broker URL")
		token := flags.String("token", "", "one-time enrollment token")
		flags.Parse(os.Args[2:])
		if *server == "" || *token == "" {
			fmt.Fprintln(os.Stderr, "server and token required")
			os.Exit(2)
		}
		if err := enroll(*server, *token); err != nil {
			slog.Error("enrollment failed", "error", err)
			os.Exit(1)
		}
	case "run":
		if err := runAgentCommand(); err != nil {
			slog.Error("agent stopped", "error", err)
			os.Exit(1)
		}
	case "service":
		if err := serviceCommand(os.Args[2:]); err != nil {
			slog.Error("service command failed", "error", err)
			os.Exit(1)
		}
	case "internal-update-helper":
		if err := updateHelper(os.Args[2:]); err != nil {
			slog.Error("update helper failed", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}

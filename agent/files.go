package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const fileChunkSize = 32768 // Base64 and JSON stay below the 64 KiB frame limit.

type fileEntry struct {
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}
type fileStream struct {
	mu        sync.Mutex
	root      *os.Root
	file      *os.File
	temp      string
	target    string
	overwrite bool
	size      int64
	received  int64
	hash      io.Writer // SHA-256 hash.Hash, kept behind the mutex
	sum       func() string
	cancelled chan struct{}
	ack       chan struct{}
	once      sync.Once
}
type fileMux struct {
	mu      sync.Mutex
	streams map[string]*fileStream
	send    func(message) error
}

func newFileMux(send func(message) error) *fileMux {
	return &fileMux{streams: make(map[string]*fileStream), send: send}
}
func (m *fileMux) reply(id, typ, code string) {
	_ = m.send(message{Version: 1, Type: typ, TransferID: id, Code: code})
}
func fileError(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "path_not_found"
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission_denied"
	}
	if errors.Is(err, os.ErrExist) {
		return "destination_exists"
	}
	return "transfer_failed"
}
func validPosixPart(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "\x00/")
}

func validWindowsPart(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "\x00/\\:") && !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, " ")
}

func windowsLocalPath(path string) (string, string, bool) {
	if len(path) < 3 || (path[0] < 'A' || path[0] > 'Z') && (path[0] < 'a' || path[0] > 'z') || path[1] != ':' || path[2] != '\\' || strings.Contains(path[3:], "/") {
		return "", "", false
	}
	drive := strings.ToUpper(path[:1]) + ":\\"
	rel := strings.TrimSuffix(path[3:], "\\")
	if rel != "" {
		for _, part := range strings.Split(rel, "\\") {
			if !validWindowsPart(part) {
				return "", "", false
			}
		}
	}
	if rel == "" {
		rel = "."
	}
	return drive, rel, true
}

// The browser never supplies a root path to os.OpenRoot unchecked. Root methods
// resolve beneath an already opened local drive/root and resist symlink swaps.
func fileRoot(path string) (*os.Root, string, error) {
	if len(path) > 4096 || strings.ContainsRune(path, 0) {
		return nil, "", os.ErrInvalid
	}
	if runtime.GOOS == "windows" {
		drive, rel, valid := windowsLocalPath(path)
		if !valid || !localDrive(drive) {
			return nil, "", os.ErrInvalid
		}
		root, err := os.OpenRoot(drive)
		return root, strings.ReplaceAll(rel, "\\", string(filepath.Separator)), err
	}
	if !strings.HasPrefix(path, "/") {
		return nil, "", os.ErrInvalid
	}
	rel := strings.Trim(path, "/")
	if rel != "" {
		for _, part := range strings.Split(rel, "/") {
			if !validPosixPart(part) {
				return nil, "", os.ErrInvalid
			}
		}
	}
	root, err := os.OpenRoot("/")
	if rel == "" {
		rel = "."
	}
	return root, rel, err
}
func (m *fileMux) add(id string, stream *fileStream) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(id) != 36 || m.streams[id] != nil || len(m.streams) >= 4 {
		return false
	}
	m.streams[id] = stream
	return true
}
func (m *fileMux) close(id string) {
	m.mu.Lock()
	stream := m.streams[id]
	delete(m.streams, id)
	m.mu.Unlock()
	if stream == nil {
		return
	}
	stream.once.Do(func() { close(stream.cancelled) })
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.file != nil {
		_ = stream.file.Close()
		stream.file = nil
	}
	if stream.temp != "" && stream.root != nil {
		_ = stream.root.Remove(stream.temp)
	}
	if stream.root != nil {
		_ = stream.root.Close()
	}
}
func (m *fileMux) closeAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.streams))
	for id := range m.streams {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.close(id)
	}
}
func (m *fileMux) list(msg message) {
	if runtime.GOOS == "windows" && msg.Path == "" {
		m.send(message{Version: 1, Type: "file_list_result", TransferID: msg.TransferID, Path: "", Entries: localDrives()})
		return
	}
	if runtime.GOOS == "linux" && msg.Path == "" {
		msg.Path = "/"
	}
	root, rel, err := fileRoot(msg.Path)
	if err != nil {
		m.reply(msg.TransferID, "file_error", "invalid_path")
		return
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if err != nil {
		m.reply(msg.TransferID, "file_error", fileError(err))
		return
	}
	if !info.IsDir() {
		m.reply(msg.TransferID, "file_error", "not_a_directory")
		return
	}
	dir, err := root.OpenFile(rel, os.O_RDONLY|safeReadFlag(), 0)
	if err != nil {
		m.reply(msg.TransferID, "file_error", fileError(err))
		return
	}
	defer dir.Close()
	// Bound a listing so no filesystem can produce an unbounded WebSocket frame.
	entries := make([]fileEntry, 0, 32)
	seen, more := 0, false
	for {
		batch, e := dir.ReadDir(1)
		if e == io.EOF {
			break
		}
		if e != nil {
			m.reply(msg.TransferID, "file_error", fileError(e))
			return
		}
		entry := batch[0]
		meta, e := entry.Info()
		if e != nil || (!meta.IsDir() && !meta.Mode().IsRegular()) || meta.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if seen < msg.Offset {
			seen++
			continue
		}
		if len(entries) == 32 {
			more = true
			break
		}
		seen++
		kind := "file"
		if meta.IsDir() {
			kind = "directory"
		}
		entries = append(entries, fileEntry{entry.Name(), kind, meta.Size(), meta.ModTime()})
	}
	_ = m.send(message{Version: 1, Type: "file_list_result", TransferID: msg.TransferID, Path: msg.Path, Entries: entries, More: more, Offset: msg.Offset})
}
func (m *fileMux) upload(msg message) {
	validName := validPosixPart(msg.Name)
	if runtime.GOOS == "windows" {
		validName = validWindowsPart(msg.Name)
	}
	if !validName || msg.Size < 0 {
		m.reply(msg.TransferID, "file_error", "invalid_path")
		return
	}
	root, rel, err := fileRoot(msg.Path)
	if err != nil {
		m.reply(msg.TransferID, "file_error", "invalid_path")
		return
	}
	info, err := root.Lstat(rel)
	if err != nil || !info.IsDir() {
		root.Close()
		m.reply(msg.TransferID, "file_error", "not_a_directory")
		return
	}
	// Open the destination directory as a rooted handle; subsequent writes and
	// final rename stay in that directory even if an ancestor is swapped.
	dir, err := root.OpenRoot(rel)
	root.Close()
	if err != nil {
		m.reply(msg.TransferID, "file_error", fileError(err))
		return
	}
	if info, err := dir.Lstat(msg.Name); err == nil {
		if !info.Mode().IsRegular() {
			dir.Close()
			m.reply(msg.TransferID, "file_error", "invalid_path")
			return
		}
		if !msg.Overwrite {
			dir.Close()
			m.reply(msg.TransferID, "file_error", "destination_exists")
			return
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		dir.Close()
		m.reply(msg.TransferID, "file_error", fileError(err))
		return
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		dir.Close()
		m.reply(msg.TransferID, "file_error", "transfer_failed")
		return
	}
	temp := ".jump-" + hex.EncodeToString(random) + ".tmp"
	file, err := dir.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		dir.Close()
		m.reply(msg.TransferID, "file_error", fileError(err))
		return
	}
	hash := sha256.New()
	stream := &fileStream{root: dir, file: file, temp: temp, target: msg.Name, overwrite: msg.Overwrite, size: msg.Size, hash: hash, sum: func() string { return hex.EncodeToString(hash.Sum(nil)) }, cancelled: make(chan struct{})}
	if !m.add(msg.TransferID, stream) {
		file.Close()
		dir.Remove(temp)
		dir.Close()
		m.reply(msg.TransferID, "file_error", "transfer_failed")
		return
	}
	m.reply(msg.TransferID, "file_opened", "")
}
func (m *fileMux) chunk(msg message) {
	m.mu.Lock()
	stream := m.streams[msg.TransferID]
	m.mu.Unlock()
	if stream == nil {
		m.reply(msg.TransferID, "file_error", "transfer_failed")
		return
	}
	data, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil || len(data) == 0 || len(data) > fileChunkSize {
		m.close(msg.TransferID)
		m.reply(msg.TransferID, "file_error", "transfer_failed")
		return
	}
	stream.mu.Lock()
	if stream.file == nil || stream.received+int64(len(data)) > stream.size {
		err = os.ErrInvalid
	} else {
		var n int
		n, err = stream.file.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err == nil {
			_, err = stream.hash.Write(data)
			stream.received += int64(n)
		}
	}
	stream.mu.Unlock()
	if err != nil {
		m.close(msg.TransferID)
		m.reply(msg.TransferID, "file_error", "transfer_failed")
		return
	}
	// Acknowledgment bounds sender to one chunk in flight.
	_ = m.send(message{Version: 1, Type: "file_opened", TransferID: msg.TransferID, Size: stream.received})
}
func (m *fileMux) finish(msg message) {
	m.mu.Lock()
	stream := m.streams[msg.TransferID]
	m.mu.Unlock()
	if stream == nil {
		return
	}
	stream.mu.Lock()
	code := ""
	sum := ""
	select {
	case <-stream.cancelled:
		code = "transfer_failed"
	default:
	}
	if code != "" || stream.file == nil || stream.received != stream.size {
		code = "transfer_failed"
	} else if err := stream.file.Sync(); err != nil {
		code = "transfer_failed"
	}
	if stream.file != nil {
		if err := stream.file.Close(); err != nil {
			code = "transfer_failed"
		}
		stream.file = nil
	}
	if code == "" {
		if !stream.overwrite {
			if err := stream.root.Link(stream.temp, stream.target); err != nil {
				code = fileError(err)
			} else if err := stream.root.Remove(stream.temp); err != nil {
				// The completed target is safe; retain the temporary name for cleanup.
				code = "transfer_failed"
			}
		} else if err := stream.root.Rename(stream.temp, stream.target); err != nil {
			code = fileError(err)
		}
		if code == "" {
			sum = stream.sum()
			stream.temp = ""
		}
	}
	size := stream.received
	stream.mu.Unlock()
	m.close(msg.TransferID)
	if code != "" {
		m.reply(msg.TransferID, "file_error", code)
	} else {
		_ = m.send(message{Version: 1, Type: "file_finished", TransferID: msg.TransferID, Size: size, SHA256: sum})
	}
}
func (m *fileMux) download(msg message) {
	root, rel, err := fileRoot(msg.Path)
	if err != nil {
		m.reply(msg.TransferID, "file_error", "invalid_path")
		return
	}
	info, err := root.Lstat(rel)
	if err != nil {
		root.Close()
		m.reply(msg.TransferID, "file_error", fileError(err))
		return
	}
	if !info.Mode().IsRegular() {
		root.Close()
		m.reply(msg.TransferID, "file_error", "not_a_regular_file")
		return
	}
	file, err := root.OpenFile(rel, os.O_RDONLY|safeReadFlag(), 0)
	if err != nil {
		root.Close()
		m.reply(msg.TransferID, "file_error", fileError(err))
		return
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		file.Close()
		root.Close()
		m.reply(msg.TransferID, "file_error", "not_a_regular_file")
		return
	}
	stream := &fileStream{root: root, file: file, size: stat.Size(), cancelled: make(chan struct{}), ack: make(chan struct{}, 1)}
	if !m.add(msg.TransferID, stream) {
		file.Close()
		root.Close()
		m.reply(msg.TransferID, "file_error", "transfer_failed")
		return
	}
	go func() {
		defer m.close(msg.TransferID)
		if m.send(message{Version: 1, Type: "file_opened", TransferID: msg.TransferID, Size: stat.Size(), Name: filepath.Base(msg.Path)}) != nil {
			return
		}
		hash := sha256.New()
		buf := make([]byte, fileChunkSize)
		var count int64
		for {
			select {
			case <-stream.cancelled:
				return
			default:
			}
			stream.mu.Lock()
			if stream.file == nil {
				stream.mu.Unlock()
				return
			}
			n, e := stream.file.Read(buf)
			stream.mu.Unlock()
			if n > 0 {
				select {
				case <-stream.cancelled:
					return
				default:
				}
				hash.Write(buf[:n])
				count += int64(n)
				if m.send(message{Version: 1, Type: "file_chunk", TransferID: msg.TransferID, Data: base64.StdEncoding.EncodeToString(buf[:n])}) != nil {
					return
				}
				select {
				case <-stream.ack:
				case <-stream.cancelled:
					return
				}
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				m.reply(msg.TransferID, "file_error", "transfer_failed")
				return
			}
		}
		if count != stat.Size() {
			m.reply(msg.TransferID, "file_error", "transfer_failed")
			return
		}
		select {
		case <-stream.cancelled:
			return
		default:
		}
		_ = m.send(message{Version: 1, Type: "file_finished", TransferID: msg.TransferID, Size: count, SHA256: hex.EncodeToString(hash.Sum(nil))})
	}()
}
func (m *fileMux) handle(msg message) bool {
	switch msg.Type {
	case "file_list":
		m.list(msg)
	case "file_upload_open":
		m.upload(msg)
	case "file_chunk":
		m.chunk(msg)
	case "file_finish":
		m.finish(msg)
	case "file_download_open":
		m.download(msg)
	case "file_cancel":
		m.close(msg.TransferID)
	case "file_ack":
		m.mu.Lock()
		stream := m.streams[msg.TransferID]
		m.mu.Unlock()
		if stream != nil && stream.ack != nil {
			select {
			case stream.ack <- struct{}{}:
			default:
			}
		}
	default:
		return false
	}
	return true
}

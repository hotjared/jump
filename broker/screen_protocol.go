package main

import (
	"encoding/base64"
	"fmt"
)

const screenChunkBytes = 16384
const screenMaxFrame = 512 * 1024
const screenMaxWidth = 1920
const screenMaxHeight = 1080

type screenInput struct {
	Action string `json:"action"`
	X      int    `json:"x,omitempty"`
	Y      int    `json:"y,omitempty"`
	Button int    `json:"button,omitempty"`
	Key    int    `json:"key,omitempty"`
	Down   bool   `json:"down,omitempty"`
	Delta  int    `json:"delta,omitempty"`
}

func validScreenID(id string) bool {
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
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validScreenKey(k int) bool {
	return k >= 0x30 && k <= 0x39 || k >= 0x41 && k <= 0x5a || k >= 0x70 && k <= 0x87 ||
		k >= 0x25 && k <= 0x28 || k >= 0xa0 && k <= 0xa5 ||
		k == 8 || k == 9 || k == 13 || k == 16 || k == 17 || k == 18 || k == 27 || k == 32 ||
		k == 33 || k == 34 || k == 35 || k == 36 || k == 45 || k == 46 || k == 91 || k == 92 ||
		k == 186 || k == 187 || k == 188 || k == 189 || k == 190 || k == 191 || k == 192 || k == 219 || k == 220 || k == 221 || k == 222
}

func validScreenInput(in *screenInput) bool {
	if in == nil || in.X < 0 || in.X > 65535 || in.Y < 0 || in.Y > 65535 {
		return false
	}
	switch in.Action {
	case "move":
		return in.Key == 0 && in.Delta == 0 && in.Button == 0 && !in.Down
	case "button":
		return in.Button >= 0 && in.Button <= 2 && in.Key == 0 && in.Delta == 0
	case "wheel":
		return in.Delta >= -1200 && in.Delta <= 1200 && in.Delta != 0 && in.Key == 0 && in.Button == 0 && !in.Down
	case "key":
		return validScreenKey(in.Key) && in.X == 0 && in.Y == 0 && in.Delta == 0 && in.Button == 0
	case "release":
		return in.X == 0 && in.Y == 0 && in.Key == 0 && in.Delta == 0 && in.Button == 0 && !in.Down
	}
	return false
}

// The route owns this validator. No partial frame can be reused after closure.
type screenSequence struct {
	last    uint64
	current uint64
	index   int
	count   int
	bytes   int
	width   int
	height  int
}

func (s *screenSequence) chunk(m message) error {
	if m.FrameID == 0 || m.Count < 1 || m.Count > screenMaxFrame/screenChunkBytes || m.Index < 0 || m.Index >= m.Count || m.Width < 1 || m.Width > screenMaxWidth || m.Height < 1 || m.Height > screenMaxHeight || len(m.Data) > 21848 {
		return fmt.Errorf("invalid_frame")
	}
	b, err := base64.StdEncoding.DecodeString(m.Data)
	if err != nil || len(b) == 0 || len(b) > screenChunkBytes || m.Index < m.Count-1 && len(b) != screenChunkBytes {
		return fmt.Errorf("invalid_frame")
	}
	if m.Index == 0 {
		if s.current != 0 || m.FrameID <= s.last {
			return fmt.Errorf("invalid_frame")
		}
		s.current = m.FrameID
		s.count, s.width, s.height, s.bytes, s.index = m.Count, m.Width, m.Height, 0, 0
	}
	if m.FrameID != s.current || m.Index != s.index || m.Count != s.count || m.Width != s.width || m.Height != s.height {
		return fmt.Errorf("invalid_frame")
	}
	s.bytes += len(b)
	s.index++
	if s.bytes > screenMaxFrame {
		return fmt.Errorf("invalid_frame")
	}
	if s.index == s.count {
		s.last = s.current
		s.current = 0
	}
	return nil
}

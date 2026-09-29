package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestPrivateDiagnostics(t *testing.T) {
	b := &broker{token: "a-long-private-token", active: map[string]*websocket.Conn{"private-device-id": nil},
		sessions: map[string]*sessionRoute{"private-session-id": nil}, files: map[string]*sessionRoute{"private-file-id": nil},
		updating: map[string]bool{"private-update-id": true}}
	for _, tc := range []struct {
		token, origin string
		status        int
	}{
		{"", "", 401}, {"Bearer wrong", "", 401}, {"Bearer a-long-private-token", "https://browser.example", 401},
		{"Bearer a-long-private-token", "", 200},
	} {
		r := httptest.NewRequest(http.MethodGet, "/internal/diagnostics", nil)
		r.Header.Set("Authorization", tc.token)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		b.diagnostics(w, r)
		if w.Code != tc.status {
			t.Fatalf("status %d, want %d", w.Code, tc.status)
		}
		if tc.status == 200 {
			var got map[string]int
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != 4 || got["agent_connections"] != 1 || got["session_routes"] != 1 || got["file_routes"] != 1 || got["updates_in_progress"] != 1 {
				t.Fatalf("counts: %v", got)
			}
			for _, secret := range []string{"private-device-id", "private-session-id", "private-file-id", "private-update-id", b.token} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatal("private data in response")
				}
			}
		}
	}
}

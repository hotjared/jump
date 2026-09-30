package main

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"image/jpeg"
	"slices"
	"testing"
	"time"
)

func TestCaptureDimensions(t *testing.T) {
	for _, v := range [][4]int{
		{1920, 1080, 1920, 1080},
		{3840, 2160, 1920, 1080},
		{1600, 1200, 1440, 1080},
		{1080, 1920, 607, 1080},
		{16384, 16384, 1080, 1080},
		{320, 240, 320, 240},
		{1, 16384, 1, 1080},
		{16384, 1, 1920, 1},
	} {
		w, h, err := captureDimensions(v[0], v[1])
		if err != nil || w != v[2] || h != v[3] {
			t.Fatalf("dimensions: %v -> %dx%d (%v)", v, w, h, err)
		}
	}
	for _, v := range [][2]int{{0, 1080}, {1920, 0}, {-1, 1}, {16385, 1}, {1, 16385}} {
		if _, _, err := captureDimensions(v[0], v[1]); err == nil || screenFailure(err) != "capture_invalid_dimensions" {
			t.Fatal("invalid dimensions accepted")
		}
	}
}

func TestCaptureBlitBoundedFallback(t *testing.T) {
	for _, outcomes := range [][2]bool{{true, true}, {false, true}, {false, false}} {
		var calls []bool
		ok := captureBlit(func(layered bool) bool {
			calls = append(calls, layered)
			return outcomes[len(calls)-1]
		})
		wantCalls := []bool{true}
		if !outcomes[0] {
			wantCalls = append(wantCalls, false)
		}
		if !slices.Equal(calls, wantCalls) || ok != (outcomes[0] || outcomes[1]) {
			t.Fatalf("fallback: calls=%v ok=%v", calls, ok)
		}
	}
}

func TestCaptureTopDownBGRXAndJPEGHandoff(t *testing.T) {
	// Two odd-width, DWORD-aligned rows. GDI's alpha/X byte is ignored.
	pixels := []byte{0, 0, 255, 0, 0, 255, 0, 17, 255, 0, 0, 255,
		255, 255, 255, 0, 0, 0, 0, 17, 0, 255, 255, 255}
	before := bytes.Clone(pixels)
	img, err := captureImage(pixels, 3, 2)
	if err != nil || img.Stride != 12 {
		t.Fatalf("pixel conversion: %v", err)
	}
	if img.RGBAAt(0, 0) != (color.RGBA{255, 0, 0, 255}) ||
		img.RGBAAt(0, 1) != (color.RGBA{255, 255, 255, 255}) ||
		img.RGBAAt(2, 1) != (color.RGBA{255, 255, 0, 255}) {
		t.Fatal("wrong channel order, alpha, stride or row orientation")
	}
	if !bytes.Equal(before, pixels) {
		t.Fatal("modified GDI-owned memory")
	}
	frame, err := encodeScreenFrame(img)
	if err != nil || len(frame.JPEG) == 0 || len(frame.JPEG) > screenMaxFrame {
		t.Fatalf("JPEG handoff: %v", err)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(frame.JPEG))
	if err != nil || config.Width != 3 || config.Height != 2 {
		t.Fatalf("JPEG dimensions: %v", err)
	}
	for _, data := range [][]byte{nil, pixels[:len(pixels)-1], append(bytes.Clone(pixels), 0)} {
		if _, err := captureImage(data, 3, 2); err == nil || err.Error() != "capture_pixels_failed" {
			t.Fatal("invalid pixel buffer accepted")
		}
	}
	if _, err := captureImage(nil, screenMaxWidth+1, 1); err == nil {
		t.Fatal("unbounded pixel allocation")
	}
}

func TestCaptureResourceCleanupAtFailureStages(t *testing.T) {
	cases := []struct {
		stage string
		owned screenCaptureResources
		calls []string
	}{
		{"capture_get_dc_failed", screenCaptureResources{}, nil},
		{"capture_create_dc_failed", screenCaptureResources{source: 1}, []string{"source"}},
		{"capture_create_bitmap_failed", screenCaptureResources{source: 1, dc: 2}, []string{"dc", "source"}},
		{"capture_select_bitmap_failed", screenCaptureResources{source: 1, dc: 2, bitmap: 3}, []string{"dc", "bitmap", "source"}},
		{"capture_blit_failed", screenCaptureResources{source: 1, dc: 2, bitmap: 3, previous: 4}, []string{"restore", "dc", "bitmap", "source"}},
		{"capture_pixels_failed", screenCaptureResources{source: 1, dc: 2, bitmap: 3, previous: 4}, []string{"restore", "dc", "bitmap", "source"}},
		{"capture_encode_failed", screenCaptureResources{source: 1, dc: 2, bitmap: 3, previous: 4}, []string{"restore", "dc", "bitmap", "source"}},
	}
	for _, c := range cases {
		t.Run(c.stage, func(t *testing.T) {
			for _, restoreFails := range []bool{false, true} {
				r := c.owned
				selected := r.previous != 0
				var calls []string
				restore := func(dc, previous uintptr) {
					if dc != 2 || previous != 4 {
						t.Fatal("incorrect selection restoration")
					}
					calls = append(calls, "restore")
					if !restoreFails {
						selected = false
					}
				}
				deleteDC := func(dc uintptr) {
					calls = append(calls, "dc")
					selected = false
				}
				deleteBitmap := func(bitmap uintptr) {
					if selected {
						t.Fatal("deleting a selected bitmap")
					}
					calls = append(calls, "bitmap")
				}
				releaseSource := func(source uintptr) { calls = append(calls, "source") }
				r.close(restore, deleteDC, deleteBitmap, releaseSource)
				r.close(restore, deleteDC, deleteBitmap, releaseSource)
				if r != (screenCaptureResources{}) || !slices.Equal(calls, c.calls) {
					t.Fatal("resources retained, released twice, or released in the wrong order")
				}
			}
		})
	}
}

type captureFailureDesktop struct {
	code   string
	closed chan struct{}
}

func (f *captureFailureDesktop) Next(context.Context) (desktopFrame, error) {
	return desktopFrame{}, errors.New(f.code)
}
func (f *captureFailureDesktop) Input(screenInput) error {
	return nil
}
func (f *captureFailureDesktop) Close() {
	close(f.closed)
}

func TestCaptureStageMappingAndScreenFailureIsolation(t *testing.T) {
	for _, code := range []string{
		"capture_invalid_dimensions", "capture_get_dc_failed", "capture_create_dc_failed",
		"capture_create_bitmap_failed", "capture_select_bitmap_failed",
		"capture_stretch_mode_failed", "capture_blit_failed", "capture_flush_failed", "capture_pixels_failed",
		"capture_encode_failed",
	} {
		t.Run(code, func(t *testing.T) {
			f := &captureFailureDesktop{code: code, closed: make(chan struct{})}
			sent := make(chan message, 4)
			m := &screenMux{
				send:   func(v message) error { sent <- v; return nil },
				launch: func(context.Context) (desktopBridge, error) { return f, nil },
			}
			m.handle(message{Type: "screen_open", SessionID: screenTestID})
			for _, kind := range []string{"screen_opened", "screen_error"} {
				select {
				case v := <-sent:
					if v.Type != kind || kind == "screen_error" && v.Code != code {
						t.Fatal("capture stage lost")
					}
				case <-time.After(time.Second):
					t.Fatal("missing capture failure")
				}
			}
			m.closeAll()
			select {
			case <-f.closed:
			default:
				t.Fatal("capture failure leaked helper")
			}
			// A failed Screen capture must not prevent another Screen session.
			next := &fakeDesktop{frames: make(chan desktopFrame), inputs: make(chan screenInput), closed: make(chan struct{})}
			m.launch = func(context.Context) (desktopBridge, error) { return next, nil }
			m.handle(message{Type: "screen_open", SessionID: screenTestID})
			select {
			case v := <-sent:
				if v.Type != "screen_opened" {
					t.Fatal("capture failure poisoned session routing")
				}
			case <-time.After(time.Second):
				t.Fatal("could not reopen Screen session")
			}
			m.closeAll()
		})
	}
	for _, code := range []string{"capture_blit_failed: secret", "capture_unknown", "raw desktop content"} {
		if safeCaptureCode(code) || screenFailure(errors.New(code)) != "capture_failed" {
			t.Fatal("unallowlisted capture detail escaped")
		}
	}
}

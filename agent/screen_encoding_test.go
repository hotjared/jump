package main

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"math/rand"
	"testing"
)

func TestAdaptiveJPEGComplexDesktop(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	rng := rand.New(rand.NewSource(1))
	rng.Read(img.Pix)
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var normal bytes.Buffer
	if err := jpeg.Encode(&normal, img, &jpeg.Options{Quality: 65}); err != nil {
		t.Fatal(err)
	}
	if normal.Len() <= screenMaxFrame {
		t.Fatal("fixture must exceed the normal frame limit")
	}
	frame, err := encodeScreenFrame(img)
	if err != nil || len(frame.JPEG) > screenMaxFrame {
		t.Fatalf("adaptive encoding: %v (%d bytes)", err, len(frame.JPEG))
	}
	decoded, err := jpeg.DecodeConfig(bytes.NewReader(frame.JPEG))
	if err != nil || decoded.Width != frame.Width || decoded.Height != frame.Height {
		t.Fatalf("invalid JPEG: %v", err)
	}
}

func TestAdaptiveJPEGPlanAndResolutionFallback(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	attempts := 0
	frame, err := adaptiveScreenJPEG(img, func(w io.Writer, img image.Image, quality int) error {
		expectedQualities := []int{65, 50, 35, 25}
		if quality != expectedQualities[attempts%4] {
			t.Fatal("quality order")
		}
		attempts++
		if img.Bounds().Dx()*9 != img.Bounds().Dy()*16 {
			t.Fatal("aspect ratio changed")
		}
		if img.Bounds().Dx() > 960 {
			return errScreenJPEGTooLarge
		}
		_, err := w.Write([]byte("fits"))
		return err
	})
	if err != nil || frame.Width != 960 || frame.Height != 540 || attempts != 9 {
		t.Fatalf("fallback: %+v %v attempts=%d", frame, err, attempts)
	}
}

func TestAdaptiveJPEGBoundedFailure(t *testing.T) {
	for _, size := range []image.Point{{1920, 1080}, {800, 600}, {320, 240}} {
		attempts := 0
		_, err := adaptiveScreenJPEG(image.NewRGBA(image.Rect(0, 0, size.X, size.Y)), func(w io.Writer, img image.Image, quality int) error {
			attempts++
			if quality < 25 || max(img.Bounds().Dx(), img.Bounds().Dy()) < min(640, max(size.X, size.Y)) {
				t.Fatal("below minimum quality/resolution")
			}
			_, err := w.Write(make([]byte, screenMaxFrame+1))
			return err
		})
		if err == nil || attempts > 12 {
			t.Fatalf("unbounded failure: %v attempts=%d", err, attempts)
		}
	}
	attempts := 0
	_, err := adaptiveScreenJPEG(image.NewRGBA(image.Rect(0, 0, 1, 1)), func(io.Writer, image.Image, int) error {
		attempts++
		return errors.New("encoder broken")
	})
	if err == nil || attempts != 1 {
		t.Fatal("non-size encoding error retried")
	}
	var buf screenJPEGBuffer
	if _, err := buf.Write(make([]byte, screenMaxFrame+1)); !errors.Is(err, errScreenJPEGTooLarge) || buf.Len() != 0 {
		t.Fatal("oversized JPEG retained")
	}
}

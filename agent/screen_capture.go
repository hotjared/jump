package main

import (
	"errors"
	"image"
)

// Only this fixed vocabulary may leave the helper or become diagnostic history.
func safeCaptureCode(code string) bool {
	switch code {
	case "capture_invalid_dimensions", "capture_get_dc_failed", "capture_create_dc_failed",
		"capture_create_bitmap_failed", "capture_select_bitmap_failed",
		"capture_stretch_mode_failed", "capture_blit_failed", "capture_flush_failed", "capture_pixels_failed",
		"capture_encode_failed":
		return true
	}
	return false
}

func captureDimensions(width, height int) (int, int, error) {
	if width < 1 || height < 1 || width > 16384 || height > 16384 {
		return 0, 0, errors.New("capture_invalid_dimensions")
	}
	w, h := width, height
	if w > screenMaxWidth {
		w, h = screenMaxWidth, max(1, height*screenMaxWidth/width)
	}
	if h > screenMaxHeight {
		w, h = max(1, width*screenMaxHeight/height), screenMaxHeight
	}
	return w, h, nil
}

// One layered-window copy, then at most one plain screen-copy fallback.
func captureBlit(copyScreen func(layered bool) bool) bool {
	if copyScreen(true) {
		return true
	}
	return copyScreen(false)
}

// A 32-bit BI_RGB top-down DIB has DWORD-aligned BGRX rows: width * 4,
// without a row flip or additional padding. Copy out of GDI-owned storage;
// its unused alpha byte must not make the JPEG source transparent.
func captureImage(pixels []byte, width, height int) (*image.RGBA, error) {
	if width < 1 || height < 1 || width > screenMaxWidth || height > screenMaxHeight ||
		len(pixels) != width*height*4 {
		return nil, errors.New("capture_pixels_failed")
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < len(pixels); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] =
			pixels[i+2], pixels[i+1], pixels[i], 255
	}
	return img, nil
}

type screenCaptureResources struct {
	source, dc, bitmap, previous uintptr
}

// Destroying the memory DC also removes its bitmap selection if restoration
// fails. Always destroy that DC before deleting the bitmap, then release the
// borrowed screen DC. Zero ownership makes cleanup idempotent.
func (r *screenCaptureResources) close(restore func(uintptr, uintptr), deleteDC, deleteBitmap, releaseSource func(uintptr)) {
	if r.previous != 0 {
		restore(r.dc, r.previous)
	}
	if r.dc != 0 {
		deleteDC(r.dc)
	}
	if r.bitmap != 0 {
		deleteBitmap(r.bitmap)
	}
	if r.source != 0 {
		releaseSource(r.source)
	}
	*r = screenCaptureResources{}
}

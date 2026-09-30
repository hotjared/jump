package main

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"io"
)

var errScreenJPEGTooLarge = errors.New("screen JPEG exceeds frame limit")

// Stop encoding at the transport bound rather than retaining an oversized JPEG.
type screenJPEGBuffer struct{ bytes.Buffer }

func (b *screenJPEGBuffer) Write(p []byte) (int, error) {
	if len(p) > screenMaxFrame-b.Len() {
		return 0, errScreenJPEGTooLarge
	}
	return b.Buffer.Write(p)
}

func encodeScreenFrame(img image.Image) (desktopFrame, error) {
	return adaptiveScreenJPEG(img, func(w io.Writer, img image.Image, quality int) error {
		return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
	})
}

// At most twelve attempts: four qualities at each of three resolutions.
// Quality never drops below 25; the longest edge never drops below 640 pixels
// (or the captured size for smaller desktops). All scaling preserves aspect.
func adaptiveScreenJPEG(img image.Image, encode func(io.Writer, image.Image, int) error) (desktopFrame, error) {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < 1 || height < 1 || width > screenMaxWidth || height > screenMaxHeight {
		return desktopFrame{}, errors.New("capture_failed")
	}
	longest := max(width, height)
	minimum := min(longest, 640)
	previous := 0
	for _, percent := range []int{100, 75, 50} {
		target := max(minimum, longest*percent/100)
		if target == previous {
			continue
		}
		previous = target
		w, h := max(1, width*target/longest), max(1, height*target/longest)
		candidate := img
		if w != width || h != height {
			scaled := image.NewRGBA(image.Rect(0, 0, w, h))
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					scaled.Set(x, y, img.At(bounds.Min.X+x*width/w, bounds.Min.Y+y*height/h))
				}
			}
			candidate = scaled
		}
		for _, quality := range []int{65, 50, 35, 25} {
			var encoded screenJPEGBuffer
			err := encode(&encoded, candidate, quality)
			if err == nil && encoded.Len() > 0 {
				return desktopFrame{encoded.Bytes(), w, h}, nil
			}
			if err != nil && !errors.Is(err, errScreenJPEGTooLarge) {
				return desktopFrame{}, errors.New("capture_failed")
			}
		}
	}
	return desktopFrame{}, errors.New("capture_failed")
}

package image

import (
	"fmt"
	"math"

	"github.com/alikilicw/filecosystem-workers/internal/contracts"
	"github.com/davidbyttow/govips/v2/vips"
)

// The pipeline replays the browser editor's edits on the original. Each step
// mirrors the canvas code in the frontend's canvas-ops.ts, so what the editor
// previewed is what comes out — at full resolution.

// minBlock matches the editor: a 1px block would be no pixelation at all.
const minBlock = 2

// runSteps applies the steps in order. The image is brought to 8-bit sRGB
// first, which is what a browser canvas holds, so every step sees the same
// three (or four, with alpha) bands the editor worked on.
func runSteps(img *vips.ImageRef, steps []contracts.ImageStep) error {
	if img.Interpretation() != vips.InterpretationSRGB {
		if err := img.ToColorSpace(vips.InterpretationSRGB); err != nil {
			return fmt.Errorf("convert to sRGB: %w", err)
		}
	}
	if img.BandFormat() != vips.BandFormatUchar {
		if err := img.Cast(vips.BandFormatUchar); err != nil {
			return fmt.Errorf("cast to 8-bit: %w", err)
		}
	}

	for i, step := range steps {
		if err := runStep(img, step); err != nil {
			return fmt.Errorf("step %d (%s): %w", i+1, step.Op, err)
		}
	}
	return nil
}

func runStep(img *vips.ImageRef, step contracts.ImageStep) error {
	switch step.Op {
	case contracts.StepResize:
		return resizeExact(img, step.Width, step.Height)
	case contracts.StepCrop:
		x, y, w, h := clampArea(step.X, step.Y, step.Width, step.Height, img.Width(), img.Height())
		return img.ExtractArea(x, y, w, h)
	case contracts.StepRotate:
		return rotate(img, step.Degrees, step.Expand, step.Fill == "white")
	case contracts.StepFlip:
		if step.Axis == "vertical" {
			return img.Flip(vips.DirectionVertical)
		}
		return img.Flip(vips.DirectionHorizontal)
	case contracts.StepPixelate:
		return pixelate(img, step)
	case contracts.StepGrayscale:
		return grayscale(img, step.Mode == "bw", step.Threshold)
	}
	return fmt.Errorf("unknown step")
}

// resizeExact scales to exactly width × height, the way the editor does once
// it has worked out the size (aspect ratio is the editor's concern).
func resizeExact(img *vips.ImageRef, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("resize needs a width and a height")
	}
	if width == img.Width() && height == img.Height() {
		return nil
	}
	return img.ThumbnailWithSize(width, height, vips.InterestingNone, vips.SizeForce)
}

// rotate follows the canvas: quarter turns swap the sides exactly; any other
// angle rotates about the centre and either keeps the rotated bounding box
// (expand) or cuts back to the original size, losing the corners.
func rotate(img *vips.ImageRef, degrees float64, expand, white bool) error {
	turn := math.Mod(math.Mod(degrees, 360)+360, 360)
	switch turn {
	case 0:
		return nil
	case 90:
		return img.Rotate(vips.Angle90)
	case 180:
		return img.Rotate(vips.Angle180)
	case 270:
		return img.Rotate(vips.Angle270)
	}

	width, height := img.Width(), img.Height()
	background := &vips.ColorRGBA{R: 255, G: 255, B: 255, A: 255}
	if !white {
		// Transparent corners need an alpha band to be transparent in.
		if !img.HasAlpha() {
			if err := img.AddAlpha(); err != nil {
				return fmt.Errorf("add alpha: %w", err)
			}
		}
		background = &vips.ColorRGBA{}
	}

	// vips measures the angle clockwise, as a canvas with y pointing down does.
	if err := img.Similarity(1, turn, background, 0, 0, 0, 0); err != nil {
		return err
	}
	if expand {
		return nil
	}
	left := (img.Width() - width) / 2
	top := (img.Height() - height) / 2
	return img.ExtractArea(max(0, left), max(0, top), min(width, img.Width()), min(height, img.Height()))
}

// pixelate averages each block (a shrink to one pixel per block), then blows
// it back up with nearest-neighbour so the blocks stay hard. Blocks start at
// the area's own corner, like the editor's.
func pixelate(img *vips.ImageRef, step contracts.ImageStep) error {
	block := max(minBlock, step.Block)
	x, y, w, h := 0, 0, img.Width(), img.Height()
	if step.Width > 0 && step.Height > 0 {
		x, y, w, h = clampArea(step.X, step.Y, step.Width, step.Height, img.Width(), img.Height())
	}

	area, err := img.Copy()
	if err != nil {
		return err
	}
	defer area.Close()

	if err := area.ExtractArea(x, y, w, h); err != nil {
		return err
	}
	cols := int(math.Ceil(float64(w) / float64(block)))
	rows := int(math.Ceil(float64(h) / float64(block)))
	if err := area.ThumbnailWithSize(cols, rows, vips.InterestingNone, vips.SizeForce); err != nil {
		return err
	}
	if err := area.Zoom(block, block); err != nil {
		return err
	}
	// The last row and column of blocks can run past the area; trim them.
	if err := area.ExtractArea(0, 0, w, h); err != nil {
		return err
	}
	return img.Insert(area, x, y, false, nil)
}

// grayscale uses Rec. 601 luma on the stored (gamma-encoded) values, the same
// sum the editor computes per pixel. Alpha is left alone.
func grayscale(img *vips.ImageRef, blackAndWhite bool, threshold int) error {
	if img.Bands() < 3 {
		return nil // already gray
	}
	luma := []float64{0.299, 0.587, 0.114}
	if err := img.Recomb([][]float64{luma, luma, luma}); err != nil {
		return err
	}

	if blackAndWhite {
		// luma >= threshold → 255, below → 0. A steep linear ramp centred on
		// the threshold, then clipped to 0..255 by the cast, does it without
		// a per-pixel comparison op.
		const steep = 1e6
		t := float64(threshold)
		a := []float64{steep, steep, steep}
		b := []float64{-t*steep + 255, -t*steep + 255, -t*steep + 255}
		if img.HasAlpha() {
			a = append(a, 1)
			b = append(b, 0)
		}
		if err := img.Linear(a, b); err != nil {
			return err
		}
	} else {
		// A canvas rounds to the nearest level when it stores a float; the
		// cast truncates, so add the half first.
		a := []float64{1, 1, 1}
		b := []float64{0.5, 0.5, 0.5}
		if img.HasAlpha() {
			a = append(a, 1)
			b = append(b, 0)
		}
		if err := img.Linear(a, b); err != nil {
			return err
		}
	}
	return img.Cast(vips.BandFormatUchar)
}

// clampArea keeps a rectangle inside the image, the same rule as the
// editor's clampRect.
func clampArea(x, y, w, h, maxW, maxH int) (int, int, int, int) {
	x = min(max(0, x), maxW-1)
	y = min(max(0, y), maxH-1)
	w = min(max(1, w), maxW-x)
	h = min(max(1, h), maxH-y)
	return x, y, w, h
}

// encodeUnder finds the highest quality whose output fits in maxBytes by
// binary search — about seven encodes for 1..100. The decoded image is reused
// for every attempt; only the encoder runs again. When even quality 1 is too
// big, the quality-1 result is returned and the caller sees the size.
func encodeUnder(img *vips.ImageRef, format string, params contracts.ImageParams, maxBytes int64) ([]byte, string, string, error) {
	low, high := 1, 100
	var best []byte
	var mime, ext string

	for low <= high {
		mid := (low + high) / 2
		attempt := params
		attempt.Quality = mid
		data, m, e, err := encode(img, format, attempt)
		if err != nil {
			return nil, "", "", err
		}
		if int64(len(data)) <= maxBytes {
			best, mime, ext = data, m, e
			low = mid + 1
		} else {
			high = mid - 1
		}
	}

	if best != nil {
		return best, mime, ext, nil
	}
	floor := params
	floor.Quality = 1
	return encode(img, format, floor)
}

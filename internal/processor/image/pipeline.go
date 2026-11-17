package image

import (
	"fmt"
	"math"

	"github.com/alikilicw/filecosystem-workers/internal/contracts"
	"github.com/davidbyttow/govips/v2/vips"
)

// minBlock: a 1px block would be no pixelation at all.
const minBlock = 2

// applyStep runs one edit on every frame and returns a new image (the
// caller still owns and closes img) and the new frame layout. The image
// keeps its band count and depth (8 or 16 bits) wherever the step allows it,
// so an edit changes only what it is meant to.
func applyStep(img *vips.ImageRef, anim *contracts.Animation, step contracts.ImageStep) (*vips.ImageRef, *contracts.Animation, error) {
	frames, err := splitFrames(img, anim)
	if err != nil {
		return nil, nil, err
	}
	closeAll := func() {
		for _, f := range frames {
			f.Close()
		}
	}

	for i, f := range frames {
		if err := runStep(f, step); err != nil {
			closeAll()
			if len(frames) > 1 {
				return nil, nil, fmt.Errorf("frame %d: %w", i+1, err)
			}
			return nil, nil, err
		}
	}

	if len(frames) == 1 {
		return frames[0], nil, nil
	}

	// Every frame got the same edit, so they are still the same size.
	frameHeight := frames[0].Height()
	if err := frames[0].ArrayJoin(frames[1:], 1); err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("join frames: %w", err)
	}
	for _, f := range frames[1:] {
		f.Close()
	}
	next := *anim
	next.FrameHeight = frameHeight
	return frames[0], &next, nil
}

// splitFrames cuts a stacked animation into one image per frame. A still
// image is its own single frame. Every entry is a new image the caller owns.
func splitFrames(img *vips.ImageRef, anim *contracts.Animation) ([]*vips.ImageRef, error) {
	if anim == nil || anim.Frames <= 1 {
		c, err := img.Copy()
		if err != nil {
			return nil, err
		}
		return []*vips.ImageRef{c}, nil
	}
	if anim.FrameHeight*anim.Frames != img.Height() {
		return nil, fmt.Errorf("animation of %d frames of %d px does not match a %d px strip", anim.Frames, anim.FrameHeight, img.Height())
	}

	// govips crops every page of a multi-page image at once; one page as tall
	// as the strip makes ExtractArea cut exactly the rectangle asked for.
	strip, err := img.Copy()
	if err != nil {
		return nil, err
	}
	defer strip.Close()
	if err := strip.SetPageHeight(strip.Height()); err != nil {
		return nil, err
	}

	frames := make([]*vips.ImageRef, 0, anim.Frames)
	for i := 0; i < anim.Frames; i++ {
		f, err := strip.Copy()
		if err != nil {
			return nil, err
		}
		if err := f.ExtractArea(0, i*anim.FrameHeight, strip.Width(), anim.FrameHeight); err != nil {
			return nil, fmt.Errorf("extract frame %d: %w", i+1, err)
		}
		frames = append(frames, f)
	}
	return frames, nil
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
	return fmt.Errorf("unknown step %q", step.Op)
}

// resizeExact scales to exactly width × height (the editor works out the
// aspect ratio).
func resizeExact(img *vips.ImageRef, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("resize needs a width and a height")
	}
	if width == img.Width() && height == img.Height() {
		return nil
	}
	return img.ThumbnailWithSize(width, height, vips.InterestingNone, vips.SizeForce)
}

// rotate: quarter turns swap the sides exactly; any other angle rotates
// about the centre and either keeps the rotated bounding box (expand) or
// cuts back to the original size, losing the corners.
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

	// The background is given as RGBA, so a one- or two-band gray image is
	// widened to colour first; the gray values themselves do not change.
	if img.Bands() < 3 {
		target := vips.InterpretationSRGB
		if img.BandFormat() == vips.BandFormatUshort {
			target = vips.InterpretationRGB16
		}
		if err := img.ToColorSpace(target); err != nil {
			return fmt.Errorf("widen gray to colour: %w", err)
		}
	}

	full := maxValue(img)
	background := &vips.ColorRGBA{R: uint8(255), G: 255, B: 255, A: 255}
	if !white {
		// Transparent corners need an alpha band to be transparent in.
		if !img.HasAlpha() {
			if err := img.AddAlpha(); err != nil {
				return fmt.Errorf("add alpha: %w", err)
			}
		}
		background = &vips.ColorRGBA{}
	}

	width, height := img.Width(), img.Height()
	// vips measures the angle clockwise, as a canvas with y pointing down does.
	if err := img.Similarity(1, turn, background, 0, 0, 0, 0); err != nil {
		return err
	}
	// ColorRGBA is 8-bit; on a 16-bit image its 255 is not white, so the
	// corners are filled again at the image's own full scale.
	if white && full > 255 {
		if err := fillCornersWhite(img, turn, width, height, full); err != nil {
			return err
		}
	}
	if expand {
		return nil
	}
	left := (img.Width() - width) / 2
	top := (img.Height() - height) / 2
	return img.ExtractArea(max(0, left), max(0, top), min(width, img.Width()), min(height, img.Height()))
}

// fillCornersWhite repaints what a rotation uncovered on a 16-bit image: it
// rotates an all-white mask the same way and lifts everything outside it to
// full scale.
func fillCornersWhite(img *vips.ImageRef, turn float64, width, height int, full float64) error {
	mask, err := vips.Black(width, height)
	if err != nil {
		return err
	}
	defer mask.Close()
	if err := mask.Linear([]float64{1}, []float64{1}); err != nil {
		return err
	}
	if err := mask.Similarity(1, turn, &vips.ColorRGBA{}, 0, 0, 0, 0); err != nil {
		return err
	}
	// mask is 1 inside the rotated image and 0 in the corners; out =
	// img*mask + full*(1-mask), per band.
	bands := img.Bands()
	scale := make([]float64, bands)
	for i := range scale {
		scale[i] = full
	}
	inverse, err := mask.Copy()
	if err != nil {
		return err
	}
	defer inverse.Close()
	if err := inverse.Linear([]float64{-full}, []float64{full}); err != nil {
		return err
	}
	format := img.BandFormat()
	if err := img.Multiply(mask); err != nil {
		return err
	}
	if err := img.Add(inverse); err != nil {
		return err
	}
	return img.Cast(format)
}

// pixelate averages each block (a shrink to one pixel per block), then blows
// it back up with nearest-neighbour so the blocks stay hard. Blocks start at
// the area's own corner.
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

// grayscale uses Rec. 601 luma on the stored (gamma-encoded) values. Alpha is
// left alone, and the result keeps the image's depth. "bw" compares luma
// against threshold on the 0–255 scale, whatever the depth.
func grayscale(img *vips.ImageRef, blackAndWhite bool, threshold int) error {
	format := img.BandFormat()
	full := maxValue(img)
	colour := img.Bands() >= 3
	alpha := img.HasAlpha()

	if colour {
		luma := []float64{0.299, 0.587, 0.114}
		if err := img.Recomb([][]float64{luma, luma, luma}); err != nil {
			return err
		}
	} else if !blackAndWhite {
		return nil // already gray
	}

	// Gray bands: 3 after Recomb, otherwise the one gray band.
	grayBands := 1
	if colour {
		grayBands = 3
	}

	a := make([]float64, 0, 4)
	b := make([]float64, 0, 4)
	if blackAndWhite {
		// luma >= threshold → full, below → 0: a steep linear ramp centred on
		// the threshold, clipped to the range by the cast.
		const steep = 1e6
		t := float64(threshold) * full / 255
		for i := 0; i < grayBands; i++ {
			a = append(a, steep)
			b = append(b, -t*steep+full)
		}
	} else {
		// Round to the nearest level; the cast truncates.
		for i := 0; i < grayBands; i++ {
			a = append(a, 1)
			b = append(b, 0.5)
		}
	}
	if alpha {
		a = append(a, 1)
		b = append(b, 0)
	}
	if err := img.Linear(a, b); err != nil {
		return err
	}
	return img.Cast(format)
}

// maxValue is full scale for the image's depth.
func maxValue(img *vips.ImageRef) float64 {
	if img.BandFormat() == vips.BandFormatUshort {
		return 65535
	}
	return 255
}

// clampArea keeps a rectangle inside the image.
func clampArea(x, y, w, h, maxW, maxH int) (int, int, int, int) {
	x = min(max(0, x), maxW-1)
	y = min(max(0, y), maxH-1)
	w = min(max(1, w), maxW-x)
	h = min(max(1, h), maxH-y)
	return x, y, w, h
}

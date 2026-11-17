package image

import (
	"fmt"

	"github.com/alikilicw/filecosystem-workers/internal/contracts"
	"github.com/davidbyttow/govips/v2/vips"
)

// pngCompression is the zlib level for results: libvips' default, a fair
// balance of size and time. It changes how long saving takes and how big the
// file is, never the pixels.
const pngCompression = 6

// encode writes img as enc says. An animation keeps its frames and timing in
// formats that can hold them (GIF, WebP); the rest get the first frame.
func encode(img *vips.ImageRef, anim *contracts.Animation, enc contracts.Encode) ([]byte, error) {
	out, err := img.Copy()
	if err != nil {
		return nil, err
	}
	defer out.Close()

	animated := anim != nil && anim.Frames > 1
	if animated {
		if enc.Format == contracts.FormatGIF || enc.Format == contracts.FormatWebP {
			if err := setAnimation(out, anim); err != nil {
				return nil, err
			}
		} else if err := out.ExtractArea(0, 0, out.Width(), anim.FrameHeight); err != nil {
			return nil, fmt.Errorf("take first frame: %w", err)
		}
	}

	strip := !enc.KeepMetadata

	var data []byte
	switch enc.Format {
	case contracts.FormatJPEG:
		if out.HasAlpha() {
			// JPEG has no alpha; compositing onto white keeps transparent
			// areas from turning black.
			if err := out.Flatten(&vips.Color{R: 255, G: 255, B: 255}); err != nil {
				return nil, fmt.Errorf("flatten alpha: %w", err)
			}
		}
		p := vips.NewJpegExportParams()
		p.Quality = clampQuality(enc.Quality)
		p.StripMetadata = strip
		p.Interlace = enc.Progressive
		p.OptimizeCoding = true
		switch enc.Subsample {
		case "on":
			p.SubsampleMode = vips.VipsForeignSubsampleOn
		case "off":
			p.SubsampleMode = vips.VipsForeignSubsampleOff
		default:
			p.SubsampleMode = vips.VipsForeignSubsampleAuto
		}
		data, _, err = out.ExportJpeg(p)

	case contracts.FormatPNG:
		p := vips.NewPngExportParams()
		p.Compression = pngCompression
		// govips defaults to no row filter, which can double a PNG's size;
		// adaptive filtering (libvips' own default) picks the best per row.
		p.Filter = vips.PngFilterAll
		p.StripMetadata = strip
		p.Interlace = enc.Progressive
		p.Bitdepth = 8
		if out.BandFormat() == vips.BandFormatUshort && !enc.Palette {
			p.Bitdepth = 16
		}
		if enc.Palette {
			p.Palette = true
			p.Quality = clampQuality(enc.Quality)
		}
		data, _, err = out.ExportPng(p)

	case contracts.FormatWebP:
		p := vips.NewWebpExportParams()
		p.StripMetadata = strip
		p.Lossless = enc.Lossless
		p.Quality = clampQuality(enc.Quality)
		data, _, err = out.ExportWebp(p)

	case contracts.FormatGIF:
		p := vips.NewGifExportParams()
		p.StripMetadata = strip
		data, _, err = out.ExportGIF(p)

	case contracts.FormatTIFF:
		p := vips.NewTiffExportParams()
		p.StripMetadata = strip
		p.Compression = tiffCompressionParam(enc.Compression)
		p.Quality = clampQuality(enc.Quality)
		data, _, err = out.ExportTiff(p)

	case contracts.FormatAVIF:
		p := vips.NewAvifExportParams()
		p.StripMetadata = strip
		p.Lossless = enc.Lossless
		p.Quality = clampQuality(enc.Quality)
		data, _, err = out.ExportAvif(p)

	case contracts.FormatHEIC:
		// The HEIF saver has no strip option, so the metadata goes first.
		if strip {
			if err := out.RemoveMetadata(); err != nil {
				return nil, fmt.Errorf("strip metadata: %w", err)
			}
		}
		p := vips.NewHeifExportParams()
		p.Lossless = enc.Lossless
		p.Quality = clampQuality(enc.Quality)
		data, _, err = out.ExportHeif(p)

	default:
		return nil, fmt.Errorf("unsupported output format %q", enc.Format)
	}
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", enc.Format, err)
	}
	return data, nil
}

// encodeUnder finds the highest quality whose output fits in enc.TargetBytes
// by binary search — about seven encodes for 1..100. For a PNG the search
// runs over the palette quantiser, the only lever a PNG has. It returns the
// settings it landed on. When even quality 1 is too big, that result is
// returned and the caller sees the size.
func encodeUnder(img *vips.ImageRef, anim *contracts.Animation, enc contracts.Encode) ([]byte, contracts.Encode, error) {
	if enc.Format == contracts.FormatPNG {
		enc.Palette = true
	} else if !contracts.HasQuality(enc.Format) || enc.Lossless {
		data, err := encode(img, anim, enc)
		return data, enc, err
	}

	low, high := 1, 100
	var best []byte
	bestQuality := 0
	for low <= high {
		mid := (low + high) / 2
		attempt := enc
		attempt.Quality = mid
		data, err := encode(img, anim, attempt)
		if err != nil {
			return nil, enc, err
		}
		if int64(len(data)) <= enc.TargetBytes {
			best, bestQuality = data, mid
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if best != nil {
		enc.Quality = bestQuality
		return best, enc, nil
	}
	enc.Quality = 1
	data, err := encode(img, anim, enc)
	return data, enc, err
}

// setAnimation tells libvips how the stacked strip splits into frames.
func setAnimation(img *vips.ImageRef, anim *contracts.Animation) error {
	if err := img.SetPageHeight(anim.FrameHeight); err != nil {
		return err
	}
	if err := img.SetPages(anim.Frames); err != nil {
		return err
	}
	if len(anim.Delays) == anim.Frames {
		if err := img.SetPageDelay(anim.Delays); err != nil {
			return err
		}
	}
	return img.SetLoop(anim.Loop)
}

func tiffCompressionParam(name string) vips.TiffCompression {
	switch name {
	case "none":
		return vips.TiffCompressionNone
	case "deflate":
		return vips.TiffCompressionDeflate
	case "packbits":
		return vips.TiffCompressionPackbits
	case "jpeg":
		return vips.TiffCompressionJpeg
	case "zstd":
		return vips.TiffCompressionZstd
	case "webp":
		return vips.TiffCompressionWebp
	}
	return vips.TiffCompressionLzw
}

func clampQuality(q int) int {
	if q <= 0 {
		return defaultQuality
	}
	return min(100, q)
}

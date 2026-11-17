// Package image turns uploads and edits into stored files. It is the only
// place that talks to libvips.
//
// Every version is made from a lossless intermediate (a PNG of the decoded
// pixels), never from the previous version's result, so a lossy format loses
// detail once per download rather than once per edit.
package image

import (
	"fmt"
	"log/slog"

	"github.com/alikilicw/filecosystem-workers/internal/contracts"
	"github.com/davidbyttow/govips/v2/vips"
)

const (
	// previewSide bounds the preview the editor shows; the file itself is
	// always full size.
	previewSide    = 1600
	previewQuality = 82
	// intermediateCompression is the zlib level for intermediates: they are
	// written and read once each, so speed matters more than size.
	intermediateCompression = 1
)

// Output is what one job produced.
type Output struct {
	// Intermediate is the lossless pixels the next edit starts from; nil when
	// the job did not change any pixels and the input is reused.
	Intermediate []byte
	// Result is the file a user downloads; nil for a source job, whose result
	// is the original upload itself.
	Result  []byte
	Preview []byte
	// Width and Height are one frame's size.
	Width     int
	Height    int
	Animation *contracts.Animation
	Encode    contracts.Encode
}

type Processor struct {
	log *slog.Logger
}

// NewProcessor boots libvips. It must be called once per process and paired
// with Shutdown.
func NewProcessor(log *slog.Logger, concurrency int) (*Processor, error) {
	vips.LoggingSettings(func(domain string, level vips.LogLevel, msg string) {
		log.Debug("libvips", "domain", domain, "level", int(level), "message", msg)
	}, vips.LogLevelWarning)

	if err := vips.Startup(&vips.Config{ConcurrencyLevel: concurrency}); err != nil {
		return nil, fmt.Errorf("start libvips: %w", err)
	}
	log.Info("libvips ready", "version", vips.Version, "concurrency", concurrency)
	return &Processor{log: log}, nil
}

func (p *Processor) Shutdown() { vips.Shutdown() }

// Source opens an upload: it reads how the original was encoded, decodes it
// once into the intermediate every edit starts from, and makes the preview.
func (p *Processor) Source(data []byte, mime string) (Output, error) {
	format := sourceFormat(data, mime)

	params := vips.NewImportParams()
	if format == contracts.FormatGIF || format == contracts.FormatWebP {
		params.NumPages.Set(-1) // every frame of an animation
	}
	img, err := vips.LoadImageFromBuffer(data, params)
	if err != nil {
		return Output{}, fmt.Errorf("decode image: %w", err)
	}
	defer img.Close()

	anim, err := animationOf(img)
	if err != nil {
		return Output{}, err
	}
	if anim == nil {
		// EXIF orientation is baked into the pixels (and the tag dropped), so
		// every edit sees the photo the right way up.
		if err := img.AutoRotate(); err != nil {
			return Output{}, fmt.Errorf("auto rotate: %w", err)
		}
	}
	if err := normalize(img); err != nil {
		return Output{}, err
	}

	intermediate, err := exportIntermediate(img)
	if err != nil {
		return Output{}, err
	}
	preview, err := previewOf(img, anim)
	if err != nil {
		return Output{}, err
	}

	return Output{
		Intermediate: intermediate,
		Preview:      preview,
		Width:        img.Width(),
		Height:       frameHeight(img, anim),
		Animation:    anim,
		Encode:       detectEncode(data, format),
	}, nil
}

// Edit makes one version from its parent's intermediate: at most one step,
// then one encode with enc.
func (p *Processor) Edit(input []byte, anim *contracts.Animation, step *contracts.ImageStep, enc contracts.Encode) (Output, error) {
	img, err := vips.NewImageFromBuffer(input)
	if err != nil {
		return Output{}, fmt.Errorf("decode intermediate: %w", err)
	}
	defer img.Close()

	out := Output{Animation: anim}
	if step != nil {
		edited, next, err := applyStep(img, anim, *step)
		if err != nil {
			return Output{}, err
		}
		defer edited.Close()
		img, out.Animation = edited, next
		if out.Intermediate, err = exportIntermediate(img); err != nil {
			return Output{}, err
		}
	}

	if enc.TargetBytes > 0 {
		out.Result, out.Encode, err = encodeUnder(img, out.Animation, enc)
	} else {
		out.Result, err = encode(img, out.Animation, enc)
		out.Encode = enc
	}
	if err != nil {
		return Output{}, err
	}

	// The preview is made from the encoded result, so it shows what a lossy
	// format did to the image, not the pixels before it.
	if out.Preview, err = previewFromFile(out.Result); err != nil {
		return Output{}, err
	}
	out.Width, out.Height = img.Width(), frameHeight(img, out.Animation)
	return out, nil
}

// sourceFormat is the format the bytes really are, falling back to the
// declared type. "" means no worker can write it (BMP).
func sourceFormat(data []byte, mime string) string {
	switch vips.DetermineImageType(data) {
	case vips.ImageTypeJPEG:
		return contracts.FormatJPEG
	case vips.ImageTypePNG:
		return contracts.FormatPNG
	case vips.ImageTypeWEBP:
		return contracts.FormatWebP
	case vips.ImageTypeGIF:
		return contracts.FormatGIF
	case vips.ImageTypeTIFF:
		return contracts.FormatTIFF
	case vips.ImageTypeAVIF:
		return contracts.FormatAVIF
	case vips.ImageTypeHEIF:
		return contracts.FormatHEIC
	case vips.ImageTypeBMP:
		return ""
	}
	return contracts.FormatFromMime(mime)
}

// animationOf describes a loaded multi-frame image, or returns nil.
func animationOf(img *vips.ImageRef) (*contracts.Animation, error) {
	pages, pageHeight := img.Pages(), img.PageHeight()
	if pages <= 1 || pageHeight <= 0 || pageHeight*pages != img.Height() {
		return nil, nil
	}
	delays, err := img.PageDelay()
	if err != nil {
		return nil, fmt.Errorf("read frame delays: %w", err)
	}
	return &contracts.Animation{
		Frames:      pages,
		FrameHeight: pageHeight,
		Delays:      delays,
		Loop:        img.Loop(),
	}, nil
}

func frameHeight(img *vips.ImageRef, anim *contracts.Animation) int {
	if anim != nil && anim.Frames > 1 {
		return anim.FrameHeight
	}
	return img.Height()
}

// normalize leaves an image as it is when the intermediate PNG can hold it
// exactly: 8- or 16-bit RGB or gray, with or without alpha. Only what a PNG
// cannot store is converted (CMYK and other colour spaces to sRGB, floating
// point to 8-bit).
func normalize(img *vips.ImageRef) error {
	switch img.Interpretation() {
	case vips.InterpretationSRGB, vips.InterpretationBW,
		vips.InterpretationRGB16, vips.InterpretationGrey16:
	default:
		if err := img.ToColorSpace(vips.InterpretationSRGB); err != nil {
			return fmt.Errorf("convert to sRGB: %w", err)
		}
	}
	switch img.BandFormat() {
	case vips.BandFormatUchar, vips.BandFormatUshort:
		return nil
	}
	if err := img.Cast(vips.BandFormatUchar); err != nil {
		return fmt.Errorf("cast to 8-bit: %w", err)
	}
	return nil
}

// exportIntermediate writes the pixels losslessly, metadata included, so the
// result can carry it over.
func exportIntermediate(img *vips.ImageRef) ([]byte, error) {
	p := vips.NewPngExportParams()
	p.Compression = intermediateCompression
	p.StripMetadata = false
	p.Interlace = false
	p.Bitdepth = 8
	if img.BandFormat() == vips.BandFormatUshort {
		p.Bitdepth = 16
	}
	data, _, err := img.ExportPng(p)
	if err != nil {
		return nil, fmt.Errorf("encode intermediate: %w", err)
	}
	return data, nil
}

// previewOf makes the preview from decoded pixels (the first frame of an
// animation).
func previewOf(img *vips.ImageRef, anim *contracts.Animation) ([]byte, error) {
	thumb, err := img.Copy()
	if err != nil {
		return nil, err
	}
	defer thumb.Close()
	if anim != nil && anim.Frames > 1 {
		if err := thumb.SetPageHeight(thumb.Height()); err != nil {
			return nil, err
		}
		if err := thumb.ExtractArea(0, 0, thumb.Width(), anim.FrameHeight); err != nil {
			return nil, err
		}
	}
	if err := thumb.ThumbnailWithSize(previewSide, previewSide, vips.InterestingNone, vips.SizeDown); err != nil {
		return nil, fmt.Errorf("shrink preview: %w", err)
	}
	return exportPreview(thumb)
}

// previewFromFile decodes an encoded result (its first frame) at preview size.
func previewFromFile(data []byte) ([]byte, error) {
	thumb, err := vips.LoadThumbnailFromBuffer(data, previewSide, previewSide, vips.InterestingNone, vips.SizeDown, nil)
	if err != nil {
		return nil, fmt.Errorf("decode result for preview: %w", err)
	}
	defer thumb.Close()
	return exportPreview(thumb)
}

func exportPreview(img *vips.ImageRef) ([]byte, error) {
	p := vips.NewWebpExportParams()
	p.Quality = previewQuality
	p.StripMetadata = true
	data, _, err := img.ExportWebp(p)
	if err != nil {
		return nil, fmt.Errorf("encode preview: %w", err)
	}
	return data, nil
}

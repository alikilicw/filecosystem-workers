// Package image turns a source image plus a set of parameters into encoded
// bytes. It is the only place that talks to libvips.
package image

import (
	"fmt"
	"log/slog"

	"github.com/alikilicw/filecosystem-workers/internal/contracts"
	"github.com/davidbyttow/govips/v2/vips"
)

// paletteQualityThreshold is where PNG output switches to palette
// quantisation, which is the only way to meaningfully shrink a PNG.
const paletteQualityThreshold = 90

// Result is an encoded image ready to be uploaded.
type Result struct {
	Data   []byte
	Mime   string
	Ext    string
	Width  int
	Height int
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

func (p *Processor) Process(op contracts.Operation, params contracts.ImageParams, source []byte) (Result, error) {
	img, err := vips.NewImageFromBuffer(source)
	if err != nil {
		return Result{}, fmt.Errorf("decode image: %w", err)
	}
	defer img.Close()

	// EXIF orientation has to be baked in before any geometry change, or a
	// rotated phone photo comes out sideways.
	if err := img.AutoRotate(); err != nil {
		return Result{}, fmt.Errorf("auto rotate: %w", err)
	}

	if op != contracts.OpPipeline {
		return Result{}, fmt.Errorf("unknown operation %q", op)
	}
	if err := runSteps(img, params.Steps); err != nil {
		return Result{}, err
	}

	format := params.Format
	if format == "" {
		format = contracts.FormatPNG
	}

	if format == contracts.FormatJPEG && img.HasAlpha() {
		// JPEG has no alpha channel; compositing onto white keeps transparent
		// areas from turning black.
		if err := img.Flatten(&vips.Color{R: 255, G: 255, B: 255}); err != nil {
			return Result{}, fmt.Errorf("flatten alpha: %w", err)
		}
	}

	var (
		data      []byte
		mime, ext string
	)
	if params.TargetBytes > 0 {
		data, mime, ext, err = encodeUnder(img, format, params, params.TargetBytes)
	} else {
		data, mime, ext, err = encode(img, format, params)
	}
	if err != nil {
		return Result{}, err
	}

	return Result{Data: data, Mime: mime, Ext: ext, Width: img.Width(), Height: img.Height()}, nil
}

func encode(img *vips.ImageRef, format string, params contracts.ImageParams) ([]byte, string, string, error) {
	ext, mime, ok := contracts.FormatInfo(format)
	if !ok {
		return nil, "", "", fmt.Errorf("unsupported output format %q", format)
	}

	quality := params.Quality
	if quality <= 0 {
		quality = 82
	}

	var (
		data []byte
		err  error
	)

	switch format {
		case contracts.FormatJPEG:
			p := vips.NewJpegExportParams()
			p.Quality = quality
			p.StripMetadata = params.StripMetadata
			p.OptimizeCoding = true
			p.Interlace = true
			data, _, err = img.ExportJpeg(p)

		case contracts.FormatPNG:
			p := vips.NewPngExportParams()
			p.Compression = pngCompression(quality)
			p.StripMetadata = params.StripMetadata
			p.Palette = quality < paletteQualityThreshold
			p.Quality = quality
			data, _, err = img.ExportPng(p)

		case contracts.FormatWebP:
			p := vips.NewWebpExportParams()
			p.Quality = quality
			p.StripMetadata = params.StripMetadata
			data, _, err = img.ExportWebp(p)

		default:
			return nil, "", "", fmt.Errorf("unsupported output format %q", format)
	}

	if err != nil {
		return nil, "", "", fmt.Errorf("encode %s: %w", format, err)
	}
	return data, mime, ext, nil
}

// pngCompression maps the shared quality scale onto zlib levels: a lower
// requested quality means the caller wants a smaller file.
func pngCompression(quality int) int {
	switch {
	case quality >= 90:
		return 6
	case quality >= 70:
		return 8
	default:
		return 9
	}
}

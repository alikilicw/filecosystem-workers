package image

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/alikilicw/filecosystem-workers/internal/contracts"
	"github.com/davidbyttow/govips/v2/vips"
)

func encodeWith(t *testing.T, data []byte, enc contracts.Encode) []byte {
	t.Helper()
	img := load(t, data)
	out, err := encode(img, nil, enc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sourceOf(t *testing.T, data []byte, mime string) Output {
	t.Helper()
	out, err := testProcessor.Source(data, mime)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Intermediate) == 0 || len(out.Preview) == 0 {
		t.Fatal("source made no intermediate or preview")
	}
	return out
}

func edit(t *testing.T, src Output, step *contracts.ImageStep, enc contracts.Encode) Output {
	t.Helper()
	out, err := testProcessor.Edit(src.Intermediate, src.Animation, step, enc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDetectsJPEGSettings(t *testing.T) {
	src := noise(t, 64, 64)
	cases := []struct {
		quality     int
		subsample   string
		progressive bool
	}{
		{75, "on", false},
		{92, "off", true},
		{50, "on", true},
	}
	for _, c := range cases {
		data := encodeWith(t, src, contracts.Encode{Format: contracts.FormatJPEG, Quality: c.quality, Subsample: c.subsample, Progressive: c.progressive})
		got := detectEncode(data, contracts.FormatJPEG)
		if got.Quality != c.quality || got.Subsample != c.subsample || got.Progressive != c.progressive {
			t.Errorf("detect(q%d %s prog=%v) = q%d %s prog=%v", c.quality, c.subsample, c.progressive, got.Quality, got.Subsample, got.Progressive)
		}
		if !got.KeepMetadata {
			t.Error("metadata should be kept by default")
		}
	}

	// Go's encoder writes the same standard tables libjpeg does.
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 16, 16)), &jpeg.Options{Quality: 83}); err != nil {
		t.Fatal(err)
	}
	if q := detectEncode(buf.Bytes(), contracts.FormatJPEG).Quality; q != 83 {
		t.Errorf("Go-encoded quality 83 detected as %d", q)
	}
}

func TestDetectsOtherFormats(t *testing.T) {
	src := source(t, 32, 32)

	if !detectEncode(encodeWith(t, src, contracts.Encode{Format: contracts.FormatPNG, Progressive: true}), contracts.FormatPNG).Progressive {
		t.Error("interlaced PNG not detected")
	}
	if detectEncode(encodeWith(t, src, contracts.Encode{Format: contracts.FormatPNG}), contracts.FormatPNG).Progressive {
		t.Error("plain PNG detected as interlaced")
	}
	if !detectEncode(encodeWith(t, src, contracts.Encode{Format: contracts.FormatWebP, Lossless: true}), contracts.FormatWebP).Lossless {
		t.Error("lossless WebP not detected")
	}
	if got := detectEncode(encodeWith(t, src, contracts.Encode{Format: contracts.FormatWebP, Quality: 70}), contracts.FormatWebP); got.Lossless || got.Quality == 0 {
		t.Errorf("lossy WebP detected as %+v", got)
	}
	for _, c := range []string{"none", "lzw", "deflate"} {
		if got := detectEncode(encodeWith(t, src, contracts.Encode{Format: contracts.FormatTIFF, Compression: c}), contracts.FormatTIFF).Compression; got != c {
			t.Errorf("TIFF %s detected as %s", c, got)
		}
	}
	if got := detectEncode([]byte("BM...."), ""); got.Format != contracts.FormatPNG {
		t.Errorf("BMP falls back to %q, want png", got.Format)
	}
}

// An edit that only re-encodes a PNG as PNG must give back the same pixels.
func TestPNGRoundTripIsLossless(t *testing.T) {
	data := noise(t, 120, 80)
	src := sourceOf(t, data, "image/png")
	if src.Encode.Format != contracts.FormatPNG {
		t.Fatalf("format = %q", src.Encode.Format)
	}
	out := edit(t, src, nil, src.Encode)
	if out.Intermediate != nil {
		t.Error("an edit without a step should reuse the intermediate")
	}
	a, b := load(t, data), load(t, out.Result)
	for _, p := range [][2]int{{0, 0}, {37, 41}, {119, 79}} {
		pa, pb := pixel(t, a, p[0], p[1]), pixel(t, b, p[0], p[1])
		for i := range pa[:3] {
			if pa[i] != pb[i] {
				t.Fatalf("pixel %v changed: %v → %v", p, pa, pb)
			}
		}
	}
}

func TestEveryFormatEncodes(t *testing.T) {
	src := sourceOf(t, source(t, 64, 48), "image/png")
	step := &contracts.ImageStep{Op: contracts.StepResize, Width: 32, Height: 24}
	want := map[string]vips.ImageType{
		contracts.FormatJPEG: vips.ImageTypeJPEG,
		contracts.FormatPNG:  vips.ImageTypePNG,
		contracts.FormatWebP: vips.ImageTypeWEBP,
		contracts.FormatGIF:  vips.ImageTypeGIF,
		contracts.FormatTIFF: vips.ImageTypeTIFF,
		contracts.FormatAVIF: vips.ImageTypeAVIF,
		contracts.FormatHEIC: vips.ImageTypeHEIF,
	}
	for format, typ := range want {
		out := edit(t, src, step, contracts.Encode{Format: format, Quality: 80, KeepMetadata: true})
		if got := vips.DetermineImageType(out.Result); got != typ {
			t.Errorf("%s result is %v", format, got)
		}
		if out.Width != 32 || out.Height != 24 {
			t.Errorf("%s result is %d×%d", format, out.Width, out.Height)
		}
		if len(out.Preview) == 0 {
			t.Errorf("%s has no preview", format)
		}
		// The result decodes again, as the next source would.
		sourceOf(t, out.Result, "")
	}
}

func png16(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA64(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.NRGBA64{R: uint16(x * 1000), G: 12345, B: uint16(y * 997), A: 65535})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSixteenBitIsKept(t *testing.T) {
	src := sourceOf(t, png16(t, 40, 30), "image/png")
	if f := load(t, src.Intermediate).BandFormat(); f != vips.BandFormatUshort {
		t.Fatalf("intermediate band format = %v, want ushort", f)
	}

	for _, step := range []contracts.ImageStep{
		{Op: contracts.StepCrop, X: 5, Y: 5, Width: 20, Height: 20},
		{Op: contracts.StepGrayscale, Mode: "gray"},
		{Op: contracts.StepRotate, Degrees: 15, Fill: "white"},
		{Op: contracts.StepPixelate, Block: 4},
	} {
		out := edit(t, src, &step, src.Encode)
		img := load(t, out.Result)
		if img.BandFormat() != vips.BandFormatUshort {
			t.Errorf("%s: result band format = %v, want ushort", step.Op, img.BandFormat())
		}
		if step.Op == contracts.StepCrop {
			// 16-bit values survive exactly: x=5 → R 5000.
			if p := pixel(t, img, 0, 0); p[0] != 5000 || p[1] != 12345 {
				t.Errorf("cropped pixel = %v, want R 5000 G 12345", p)
			}
		}
		if step.Op == contracts.StepRotate {
			if p := pixel(t, img, 0, 0); p[0] < 65000 || p[1] < 65000 || p[2] < 65000 {
				t.Errorf("16-bit white corner = %v", p)
			}
		}
	}
}

func animatedGIF(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}, color.RGBA{0, 255, 0, 255}}
	anim := &gif.GIF{LoopCount: 0}
	for i := 0; i < 3; i++ {
		frame := image.NewPaletted(image.Rect(0, 0, 40, 30), palette)
		for p := range frame.Pix {
			frame.Pix[p] = uint8(i)
		}
		anim.Image = append(anim.Image, frame)
		anim.Delay = append(anim.Delay, 10*(i+1)) // centiseconds
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAnimatedGIFKeepsEveryFrame(t *testing.T) {
	src := sourceOf(t, animatedGIF(t), "image/gif")
	if src.Animation == nil || src.Animation.Frames != 3 || src.Animation.FrameHeight != 30 {
		t.Fatalf("animation = %+v, want 3 frames of 30 px", src.Animation)
	}
	if src.Width != 40 || src.Height != 30 {
		t.Fatalf("frame size = %d×%d", src.Width, src.Height)
	}

	step := &contracts.ImageStep{Op: contracts.StepCrop, X: 10, Y: 5, Width: 20, Height: 10}
	out := edit(t, src, step, src.Encode)
	if out.Animation == nil || out.Animation.Frames != 3 || out.Animation.FrameHeight != 10 {
		t.Fatalf("edited animation = %+v", out.Animation)
	}

	params := vips.NewImportParams()
	params.NumPages.Set(-1)
	res, err := vips.LoadImageFromBuffer(out.Result, params)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	if res.Pages() != 3 || res.PageHeight() != 10 || res.Width() != 20 {
		t.Fatalf("result = %d pages of %d×%d", res.Pages(), res.Width(), res.PageHeight())
	}
	delays, _ := res.PageDelay()
	if len(delays) != 3 || delays[0] != 100 || delays[2] != 300 {
		t.Fatalf("delays = %v, want [100 200 300]", delays)
	}
	// Each frame keeps its own colour: frame 2 is blue.
	if p := pixel(t, res, 5, 15); p[2] < 200 || p[0] > 50 {
		t.Fatalf("frame 2 pixel = %v, want blue", p)
	}

	// A still format gets the first frame.
	still := edit(t, src, nil, contracts.Encode{Format: contracts.FormatPNG, KeepMetadata: true})
	if img := load(t, still.Result); img.Width() != 40 || img.Height() != 30 {
		t.Fatalf("still result = %d×%d", img.Width(), img.Height())
	}
}

// withExif puts a minimal EXIF block (IFD0 Software = "abc") after SOI.
func withExif(t *testing.T, jpg []byte) []byte {
	t.Helper()
	var tiff bytes.Buffer
	tiff.WriteString("II*\x00")
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(1))      // one entry
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x0131)) // Software
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(2))      // ASCII
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(4))
	tiff.WriteString("abc\x00")
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0)) // no next IFD

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	seg := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	seg = append(seg, payload...)

	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

func TestMetadataIsKeptByDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 32, 32)), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	data := withExif(t, buf.Bytes())
	src := sourceOf(t, data, "image/jpeg")

	step := &contracts.ImageStep{Op: contracts.StepResize, Width: 16, Height: 16}
	kept := load(t, edit(t, src, step, src.Encode).Result)
	if !strings.Contains(kept.GetString("exif-ifd0-Software"), "abc") {
		t.Fatalf("EXIF lost: fields %v", kept.ImageFields())
	}

	stripped := src.Encode
	stripped.KeepMetadata = false
	if load(t, edit(t, src, step, stripped).Result).HasExif() {
		t.Fatal("EXIF kept although stripping was asked for")
	}
}

func TestTargetSizeFitsUnderTheLimit(t *testing.T) {
	src := sourceOf(t, noise(t, 300, 300), "image/png")
	full := edit(t, src, nil, contracts.Encode{Format: contracts.FormatJPEG, Quality: 100})

	target := int64(len(full.Result)) / 3
	out := edit(t, src, nil, contracts.Encode{Format: contracts.FormatJPEG, TargetBytes: target})
	if int64(len(out.Result)) > target {
		t.Fatalf("result is %d bytes, over the %d target", len(out.Result), target)
	}
	// It should use most of the budget, not jump to the lowest quality.
	if int64(len(out.Result)) < target/2 {
		t.Fatalf("result is %d bytes, far under the %d target", len(out.Result), target)
	}
	if out.Encode.Quality <= 1 || out.Encode.Quality >= 100 {
		t.Fatalf("landed on quality %d", out.Encode.Quality)
	}
}

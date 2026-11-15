package image

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"testing"

	"github.com/alikilicw/filecosystem-workers/internal/contracts"
	"github.com/davidbyttow/govips/v2/vips"
)

var testProcessor *Processor

func TestMain(m *testing.M) {
	p, err := NewProcessor(slog.New(slog.NewTextHandler(io.Discard, nil)), 1)
	if err != nil {
		panic(err)
	}
	testProcessor = p
	code := m.Run()
	p.Shutdown()
	os.Exit(code)
}

// source builds a PNG whose left half is red and right half is blue, so
// geometry changes can be checked by looking at where the colours went.
func source(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if x < width/2 {
				img.Set(x, y, color.NRGBA{R: 255, A: 255})
			} else {
				img.Set(x, y, color.NRGBA{B: 255, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noise is a photo stand-in: random pixels compress badly enough that the
// quality setting makes a real difference to the file size.
func noise(t *testing.T, width, height int) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for i := range img.Pix {
		img.Pix[i] = byte(rng.Intn(256))
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func load(t *testing.T, data []byte) *vips.ImageRef {
	t.Helper()
	img, err := vips.NewImageFromBuffer(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(img.Close)
	return img
}

func run(t *testing.T, img *vips.ImageRef, steps ...contracts.ImageStep) {
	t.Helper()
	if err := runSteps(img, steps); err != nil {
		t.Fatal(err)
	}
}

func size(t *testing.T, img *vips.ImageRef, width, height int) {
	t.Helper()
	if img.Width() != width || img.Height() != height {
		t.Fatalf("size = %d×%d, want %d×%d", img.Width(), img.Height(), width, height)
	}
}

func pixel(t *testing.T, img *vips.ImageRef, x, y int) []float64 {
	t.Helper()
	p, err := img.GetPoint(x, y)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func near(a, b float64) bool { return math.Abs(a-b) <= 2 }

func TestResizeIsExact(t *testing.T) {
	img := load(t, source(t, 400, 300))
	run(t, img, contracts.ImageStep{Op: contracts.StepResize, Width: 123, Height: 77})
	size(t, img, 123, 77)
}

func TestCropTakesTheArea(t *testing.T) {
	img := load(t, source(t, 400, 300))
	// The right half is blue; a crop inside it must be all blue.
	run(t, img, contracts.ImageStep{Op: contracts.StepCrop, X: 250, Y: 20, Width: 100, Height: 50})
	size(t, img, 100, 50)
	if p := pixel(t, img, 0, 0); !near(p[2], 255) || !near(p[0], 0) {
		t.Fatalf("crop pixel = %v, want blue", p)
	}
}

func TestCropIsClampedToTheImage(t *testing.T) {
	img := load(t, source(t, 400, 300))
	run(t, img, contracts.ImageStep{Op: contracts.StepCrop, X: 350, Y: 250, Width: 500, Height: 500})
	size(t, img, 50, 50)
}

func TestQuarterTurnSwapsSides(t *testing.T) {
	img := load(t, source(t, 400, 300))
	run(t, img, contracts.ImageStep{Op: contracts.StepRotate, Degrees: 90})
	size(t, img, 300, 400)
	// Clockwise: the red left half ends up on top.
	if p := pixel(t, img, 150, 10); !near(p[0], 255) {
		t.Fatalf("top pixel after +90 = %v, want red", p)
	}

	img = load(t, source(t, 400, 300))
	run(t, img, contracts.ImageStep{Op: contracts.StepRotate, Degrees: -90})
	if p := pixel(t, img, 150, 10); !near(p[2], 255) {
		t.Fatalf("top pixel after -90 = %v, want blue", p)
	}
}

func TestStraightenExpandsToTheBoundingBox(t *testing.T) {
	img := load(t, source(t, 400, 300))
	run(t, img, contracts.ImageStep{Op: contracts.StepRotate, Degrees: 30, Expand: true})

	rad := 30 * math.Pi / 180
	wantW := 400*math.Cos(rad) + 300*math.Sin(rad)
	wantH := 400*math.Sin(rad) + 300*math.Cos(rad)
	if math.Abs(float64(img.Width())-wantW) > 2 || math.Abs(float64(img.Height())-wantH) > 2 {
		t.Fatalf("size = %d×%d, want about %.0f×%.0f", img.Width(), img.Height(), wantW, wantH)
	}
	// Transparent fill: the corner is outside the rotated image.
	if !img.HasAlpha() {
		t.Fatal("expected an alpha band for transparent corners")
	}
	if p := pixel(t, img, 0, 0); p[3] != 0 {
		t.Fatalf("corner alpha = %v, want 0", p[3])
	}
}

func TestStraightenWithoutExpandKeepsTheSize(t *testing.T) {
	img := load(t, source(t, 400, 300))
	run(t, img, contracts.ImageStep{Op: contracts.StepRotate, Degrees: -12.5, Fill: "white"})
	size(t, img, 400, 300)
	if p := pixel(t, img, 0, 0); !near(p[0], 255) || !near(p[1], 255) || !near(p[2], 255) {
		t.Fatalf("corner = %v, want white", p)
	}
}

func TestFlipMirrors(t *testing.T) {
	img := load(t, source(t, 400, 300))
	run(t, img, contracts.ImageStep{Op: contracts.StepFlip, Axis: "horizontal"})
	if p := pixel(t, img, 10, 10); !near(p[2], 255) {
		t.Fatalf("left pixel after flip = %v, want blue", p)
	}
}

func TestPixelateAreaOnly(t *testing.T) {
	img := load(t, noise(t, 200, 200))
	before := pixel(t, img, 5, 5)
	run(t, img, contracts.ImageStep{Op: contracts.StepPixelate, Block: 20, X: 100, Y: 100, Width: 60, Height: 60})
	size(t, img, 200, 200)

	// Outside the area nothing changes.
	if after := pixel(t, img, 5, 5); after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("pixel outside the area changed: %v → %v", before, after)
	}
	// Inside, one block is a single colour.
	a, b := pixel(t, img, 101, 101), pixel(t, img, 118, 118)
	if a[0] != b[0] || a[1] != b[1] || a[2] != b[2] {
		t.Fatalf("block is not uniform: %v vs %v", a, b)
	}
}

func TestGrayscaleUsesRec601(t *testing.T) {
	img := load(t, source(t, 40, 40))
	run(t, img, contracts.ImageStep{Op: contracts.StepGrayscale, Mode: "gray"})
	// Red: 0.299 × 255 = 76.2 → 76. Blue: 0.114 × 255 = 29.1 → 29.
	if p := pixel(t, img, 5, 5); p[0] != 76 || p[1] != 76 || p[2] != 76 {
		t.Fatalf("red → %v, want 76", p)
	}
	if p := pixel(t, img, 35, 5); p[0] != 29 {
		t.Fatalf("blue → %v, want 29", p)
	}
}

func TestBlackAndWhiteThreshold(t *testing.T) {
	img := load(t, source(t, 40, 40))
	// Red's luma is 76: white at threshold 50, black at 100.
	run(t, img, contracts.ImageStep{Op: contracts.StepGrayscale, Mode: "bw", Threshold: 50})
	if p := pixel(t, img, 5, 5); p[0] != 255 {
		t.Fatalf("red at threshold 50 → %v, want 255", p)
	}
	if p := pixel(t, img, 35, 5); p[0] != 0 {
		t.Fatalf("blue at threshold 50 → %v, want 0", p)
	}

	img = load(t, source(t, 40, 40))
	run(t, img, contracts.ImageStep{Op: contracts.StepGrayscale, Mode: "bw", Threshold: 100})
	if p := pixel(t, img, 5, 5); p[0] != 0 {
		t.Fatalf("red at threshold 100 → %v, want 0", p)
	}
}

func TestPipelineEndToEnd(t *testing.T) {
	params := contracts.ImageParams{
		Format:  contracts.FormatJPEG,
		Quality: 80,
		Steps: []contracts.ImageStep{
			{Op: contracts.StepCrop, X: 10, Y: 10, Width: 300, Height: 200},
			{Op: contracts.StepRotate, Degrees: 90},
			{Op: contracts.StepResize, Width: 100, Height: 150},
			{Op: contracts.StepGrayscale, Mode: "gray"},
		},
	}
	out, err := testProcessor.Process(contracts.OpPipeline, params, source(t, 400, 300))
	if err != nil {
		t.Fatal(err)
	}
	if out.Width != 100 || out.Height != 150 || out.Mime != "image/jpeg" {
		t.Fatalf("result = %d×%d %s, want 100×150 image/jpeg", out.Width, out.Height, out.Mime)
	}
}

func TestTargetSizeFitsUnderTheLimit(t *testing.T) {
	src := noise(t, 300, 300)
	full, err := testProcessor.Process(contracts.OpPipeline,
		contracts.ImageParams{Format: contracts.FormatJPEG, Quality: 100}, src)
	if err != nil {
		t.Fatal(err)
	}

	target := int64(len(full.Data)) / 3
	out, err := testProcessor.Process(contracts.OpPipeline,
		contracts.ImageParams{Format: contracts.FormatJPEG, TargetBytes: target}, src)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(out.Data)) > target {
		t.Fatalf("result is %d bytes, over the %d target", len(out.Data), target)
	}
	// It should use most of the budget, not jump to the lowest quality.
	if int64(len(out.Data)) < target/2 {
		t.Fatalf("result is %d bytes, far under the %d target", len(out.Data), target)
	}
}

package image

import (
	"bytes"
	"encoding/binary"

	"github.com/alikilicw/filecosystem-workers/internal/contracts"
)

// defaultQuality is used for lossy formats whose files do not say what
// quality they were saved at (WebP, AVIF, HEIC). It is deliberately high: the
// point is to change the image as little as possible.
const defaultQuality = 90

// detectEncode works out how the original was written, so every version is
// written the same way unless the user asks otherwise. format is the source's
// own format ("" when no worker can write it, which falls back to PNG).
func detectEncode(data []byte, format string) contracts.Encode {
	enc := contracts.Encode{Format: format, KeepMetadata: true}

	switch format {
	case contracts.FormatJPEG:
		enc.Quality = defaultQuality
		if table := readLumaTable(data); table != nil {
			enc.Quality = qualityFromTable(table)
		}
		subsampled, progressive, ok := readJPEGFrame(data)
		if ok {
			enc.Subsample = "off"
			if subsampled {
				enc.Subsample = "on"
			}
			enc.Progressive = progressive
		}

	case contracts.FormatPNG:
		enc.Progressive = pngInterlaced(data)

	case contracts.FormatWebP:
		enc.Lossless = webpLossless(data)
		if !enc.Lossless {
			enc.Quality = defaultQuality
		}

	case contracts.FormatAVIF, contracts.FormatHEIC:
		enc.Quality = defaultQuality

	case contracts.FormatTIFF:
		enc.Compression = tiffCompression(data)

	case contracts.FormatGIF:
		// Nothing to choose: GIF is always a palette of at most 256 colours.

	default:
		// BMP and anything else no worker writes: PNG keeps every pixel.
		enc.Format = contracts.FormatPNG
	}
	return enc
}

// ITU-T T.81 Annex K, table K.1, in natural (row-major) order.
var standardLuma = [64]int{
	16, 11, 10, 16, 24, 40, 51, 61, 12, 12, 14, 19, 26, 58, 60, 55, 14, 13, 16, 24, 40, 57, 69, 56,
	14, 17, 22, 29, 51, 87, 80, 62, 18, 22, 37, 56, 68, 109, 103, 77, 24, 35, 55, 64, 81, 104, 113,
	92, 49, 64, 78, 87, 103, 121, 120, 101, 72, 92, 95, 98, 112, 100, 103, 99,
}

// A DQT segment stores its values in zigzag order: entry k is the
// coefficient at zigzag[k] in natural order.
var zigzag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5, 12, 19, 26, 33, 40, 48, 41, 34, 27, 20,
	13, 6, 7, 14, 21, 28, 35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51, 58, 59, 52,
	45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63,
}

// jpegSegments walks the JPEG header up to the start of scan, calling fn with
// each marker and its payload. It stops early when fn returns false.
func jpegSegments(data []byte, fn func(marker byte, payload []byte) bool) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return
	}
	offset := 2
	for offset+4 <= len(data) {
		if data[offset] != 0xff {
			return
		}
		marker := data[offset+1]
		if marker == 0xff { // fill byte
			offset++
			continue
		}
		if marker == 0xda || marker == 0xd9 { // start of scan, end of image
			return
		}
		length := int(binary.BigEndian.Uint16(data[offset+2:]))
		end := offset + 2 + length
		if length < 2 || end > len(data) {
			return
		}
		if !fn(marker, data[offset+4:end]) {
			return
		}
		offset = end
	}
}

// readLumaTable returns the luminance quantisation table (id 0) in natural
// order, or nil when the header has none.
func readLumaTable(data []byte) []int {
	var table []int
	jpegSegments(data, func(marker byte, p []byte) bool {
		if marker != 0xdb {
			return true
		}
		// One DQT segment may hold several tables.
		for i := 0; i < len(p); {
			precision, id := p[i]>>4, p[i]&0x0f
			i++
			size := 1
			if precision != 0 {
				size = 2
			}
			if i+64*size > len(p) {
				return false
			}
			if id == 0 {
				table = make([]int, 64)
				for k := 0; k < 64; k++ {
					if size == 1 {
						table[zigzag[k]] = int(p[i+k])
					} else {
						table[zigzag[k]] = int(binary.BigEndian.Uint16(p[i+2*k:]))
					}
				}
				return false
			}
			i += 64 * size
		}
		return true
	})
	return table
}

// qualityFromTable is the libjpeg quality (1–100) whose scaled standard table
// is closest to table. Most encoders scale Annex K this way; for cameras and
// Photoshop, which use tables of their own, it is the nearest estimate.
func qualityFromTable(table []int) int {
	best, bestErr := 1, -1
	for q := 1; q <= 100; q++ {
		scale := 200 - q*2
		if q < 50 {
			scale = 5000 / q
		}
		e := 0
		for k := 0; k < 64; k++ {
			v := (standardLuma[k]*scale + 50) / 100
			v = min(255, max(1, v))
			d := v - table[k]
			if d < 0 {
				d = -d
			}
			e += d
		}
		// <= so a tie goes to the higher quality: never estimate low.
		if bestErr < 0 || e <= bestErr {
			best, bestErr = q, e
		}
	}
	return best
}

// readJPEGFrame reads the start-of-frame header: whether chroma is stored at
// a lower resolution than luma, and whether the file is progressive.
func readJPEGFrame(data []byte) (subsampled, progressive, ok bool) {
	jpegSegments(data, func(marker byte, p []byte) bool {
		// SOF0..SOF15, except DHT (c4), JPG (c8) and DAC (cc).
		if marker < 0xc0 || marker > 0xcf || marker == 0xc4 || marker == 0xc8 || marker == 0xcc {
			return true
		}
		if len(p) < 6 {
			return false
		}
		components := int(p[5])
		if len(p) < 6+3*components || components == 0 {
			return false
		}
		progressive = marker == 0xc2 || marker == 0xc6 || marker == 0xca || marker == 0xce
		lumaH, lumaV := int(p[7]>>4), int(p[7]&0x0f)
		for c := 1; c < components; c++ {
			h, v := int(p[6+3*c+1]>>4), int(p[6+3*c+1]&0x0f)
			if h < lumaH || v < lumaV {
				subsampled = true
			}
		}
		ok = true
		return false
	})
	return subsampled, progressive, ok
}

// pngInterlaced reads the interlace method from IHDR.
func pngInterlaced(data []byte) bool {
	// 8-byte signature, 4-byte length, "IHDR", then 13 bytes of which the
	// last is the interlace method.
	if len(data) < 33 || !bytes.Equal(data[12:16], []byte("IHDR")) {
		return false
	}
	return data[28] == 1
}

// webpLossless reports whether the first image chunk is VP8L.
func webpLossless(data []byte) bool {
	if len(data) < 16 || !bytes.Equal(data[0:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return false
	}
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4:]))
		switch id {
		case "VP8L":
			return true
		case "VP8 ":
			return false
		case "ANMF":
			// An animation frame holds its image chunk after a 16-byte header.
			inner := offset + 8 + 16
			if inner+4 <= len(data) {
				return string(data[inner:inner+4]) == "VP8L"
			}
			return false
		}
		offset += 8 + size + size%2
	}
	return false
}

// tiffCompression reads tag 259 from the first IFD.
func tiffCompression(data []byte) string {
	if len(data) < 8 {
		return "lzw"
	}
	var order binary.ByteOrder
	switch string(data[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return "lzw"
	}
	ifd := int(order.Uint32(data[4:8]))
	if ifd+2 > len(data) {
		return "lzw"
	}
	entries := int(order.Uint16(data[ifd:]))
	for i := 0; i < entries; i++ {
		e := ifd + 2 + 12*i
		if e+12 > len(data) {
			break
		}
		if order.Uint16(data[e:]) != 259 {
			continue
		}
		switch order.Uint16(data[e+8:]) {
		case 1:
			return "none"
		case 5:
			return "lzw"
		case 7:
			return "jpeg"
		case 8, 32946:
			return "deflate"
		case 32773:
			return "packbits"
		case 50000:
			return "zstd"
		case 50001:
			return "webp"
		}
		break
	}
	return "lzw"
}

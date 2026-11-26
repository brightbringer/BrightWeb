package packer

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"ytcloud/internal/media"
)

func buildTestWAV(seconds int) []byte {
	sr := 44100
	dataBytes := sr * seconds * 4
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+dataBytes))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sr))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sr*4))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(4))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataBytes))
	for i := 0; i < sr*seconds; i++ {
		t := float64(i) / float64(sr)
		l := int16(14000 * math.Sin(2*math.Pi*220*t))
		r := int16(12000 * math.Sin(2*math.Pi*330*t))
		_ = binary.Write(&buf, binary.LittleEndian, l)
		_ = binary.Write(&buf, binary.LittleEndian, r)
	}
	return buf.Bytes()
}

func TestSolidZstdDeltaPCM16AndWebPLosslessRoundTrip(t *testing.T) {
	tools := media.DetectTools()
	wavBytes := buildTestWAV(1)
	txt1 := bytes.Repeat([]byte("YTCloud Solid Zstd Compression Test Line 0123456789\n"), 200)
	txt2 := bytes.Repeat([]byte("YTCloud Solid Zstd Compression Test Line ABCDEFGHIJ\n"), 200)

	// Realistic 256x256 PNG
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), uint8((x + y) / 2), 255})
		}
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)
	pngBytes := pngBuf.Bytes()

	inputs := []InputFile{
		{RelPath: "docs/a.txt", Data: txt1, ModTime: "2026-01-01 10:00:00"},
		{RelPath: "docs/b.txt", Data: txt2, ModTime: "2026-01-01 10:00:00"},
		{RelPath: "audio/clip.wav", Data: wavBytes, ModTime: "2026-01-01 10:00:00"},
		{RelPath: "img/grad.png", Data: pngBytes, ModTime: "2026-01-01 10:00:00"},
	}

	manifest, payload, err := PackFilesWithOptions(inputs, PackOptions{
		ContainerID:          "test-solid",
		ProfileCode:          "y8-gray",
		ResolutionCode:       "4k",
		FFmpegPath:           tools.FFmpegPath,
		IncludeMicroPreviews: false,
		OptimizePNGLossless:  tools.HasFFmpeg,
	})
	if err != nil {
		t.Fatalf("PackFilesWithOptions failed: %v", err)
	}
	if !manifest.SolidCompressed {
		t.Fatalf("Expected SolidCompressed=true")
	}

	rawTotal := len(txt1) + len(txt2) + len(wavBytes) + len(pngBytes)
	t.Logf("Packed %d B -> %d B (%.1f%%)", rawTotal, len(payload), 100*float64(len(payload))/float64(rawTotal))
	if len(payload) >= rawTotal/2 {
		t.Fatalf("Expected strong compression (<50%%), got %d/%d", len(payload), rawTotal)
	}

	destDir := t.TempDir()
	extracted, err := ExtractFiles(manifest, payload, destDir, nil, nil, tools.FFmpegPath)
	if err != nil {
		t.Fatalf("ExtractFiles failed: %v", err)
	}
	if len(extracted) != 4 {
		t.Fatalf("Expected 4 extracted files, got %d", len(extracted))
	}

	// Verify bit-exact recovery for txt & WAV (Delta-PCM16!)
	gotWAV, _ := os.ReadFile(filepath.Join(destDir, "audio", "clip.wav"))
	if !bytes.Equal(gotWAV, wavBytes) {
		t.Fatalf("WAV round-trip mismatch! Delta-PCM16 must be 100%% bit-exact!")
	}
	gotTxt1, _ := os.ReadFile(filepath.Join(destDir, "docs", "a.txt"))
	if !bytes.Equal(gotTxt1, txt1) {
		t.Fatalf("txt1 mismatch")
	}
	// Verify pixel-exact recovery for PNG
	gotPNGBytes, _ := os.ReadFile(filepath.Join(destDir, "img", "grad.png"))
	gotImg, err := png.Decode(bytes.NewReader(gotPNGBytes))
	if err != nil {
		t.Fatalf("Restored PNG failed to decode: %v", err)
	}
	for y := 0; y < 256; y += 17 {
		for x := 0; x < 256; x += 17 {
			r1, g1, b1, a1 := img.At(x, y).RGBA()
			r2, g2, b2, a2 := gotImg.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				t.Fatalf("PNG pixel mismatch at (%d,%d)", x, y)
			}
		}
	}
}

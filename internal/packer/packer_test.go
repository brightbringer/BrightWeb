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
	if !manifest.SolidCompressed && len(manifest.SolidBlocks) == 0 {
		t.Fatalf("Expected SolidCompressed=true or SolidBlocks>0")
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

func TestHybridPerLayerPackingAndFaultIsolation(t *testing.T) {
	indexHTML := bytes.Repeat([]byte("<html><body>BrightWeb Root Layer 0</body></html>\n"), 40)
	aboutHTML := bytes.Repeat([]byte("<html><body>Subpage Layer 1</body></html>\n"), 50)
	docMD := bytes.Repeat([]byte("# Documentation Layer 2\nOptical codec details.\n"), 60)
	bigBin := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x13, 0x37, 0x99, 0x42}, 8000)

	inputs := []InputFile{
		{RelPath: "site/assets/media/hd/big.jpg", Data: bigBin, ModTime: "2026-01-01 10:00:00"},
		{RelPath: "site/assets/docs/guide.md", Data: docMD, ModTime: "2026-01-01 10:00:00"},
		{RelPath: "site/pages/about.html", Data: aboutHTML, ModTime: "2026-01-01 10:00:00"},
		{RelPath: "site/index.html", Data: indexHTML, ModTime: "2026-01-01 10:00:00"},
	}

	manifest, payload, err := PackFilesWithOptions(inputs, PackOptions{
		ContainerID:    "test-hybrid",
		ProfileCode:    "y8-gray",
		ResolutionCode: "2k",
	})
	if err != nil {
		t.Fatalf("PackFilesWithOptions failed: %v", err)
	}
	if len(manifest.SolidBlocks) != 3 {
		t.Fatalf("expected 3 independent layer solid blocks (depths 0,1,2), got %d", len(manifest.SolidBlocks))
	}
	// Root file (index.html, depth 0) MUST come first in manifest.Files!
	if manifest.Files[0].RelPath != "site/index.html" || manifest.Files[0].LayerDepth != 0 {
		t.Fatalf("expected root index.html first with depth 0, got %s (depth %d)", manifest.Files[0].RelPath, manifest.Files[0].LayerDepth)
	}

	// Corrupt the tail (depth 3 big.jpg): root index.html & layer 1 about.html MUST still extract 100% cleanly!
	damaged := append([]byte(nil), payload...)
	for i := len(damaged) - len(bigBin)/2; i < len(damaged); i++ {
		damaged[i] ^= 0xFF
	}
	gotIndex, _, err := ExtractSingleBytes(manifest, damaged, "site/index.html")
	if err != nil || !bytes.Equal(gotIndex, indexHTML) {
		t.Fatalf("root index.html failed isolation under tail corruption: err=%v", err)
	}
	gotAbout, _, err := ExtractSingleBytes(manifest, damaged, "site/pages/about.html")
	if err != nil || !bytes.Equal(gotAbout, aboutHTML) {
		t.Fatalf("layer 1 about.html failed isolation under tail corruption: err=%v", err)
	}

	// Intact ExtractAllToMemory must recover all 4 files cleanly
	all, err := ExtractAllToMemory(manifest, payload)
	if err != nil || len(all) != 4 {
		t.Fatalf("ExtractAllToMemory failed: len=%d err=%v", len(all), err)
	}
	if !bytes.Equal(all["site/assets/media/hd/big.jpg"], bigBin) {
		t.Fatalf("big.jpg mismatch")
	}
}

func TestBrightNet3CriticalPathWebDictAndZeroHoL(t *testing.T) {
	indexHTML := []byte(`<!DOCTYPE html><html lang="pt-BR"><head><meta charset="UTF-8"><title>BrightNet/3</title><link rel="stylesheet" href="assets/css/main.css"><script src="static/js/app.js"></script></head><body><h1>Hello</h1></body></html>`)
	mainCSS := []byte(`:root{--bg:#0f172a;--text:#f8fafc;}body{display:flex;flex-direction:column;background:var(--bg);color:var(--text);font-family:"Segoe UI",sans-serif;padding:12px 16px;}`)
	appJS := []byte(`"use strict";document.addEventListener("DOMContentLoaded",()=>{const el=document.getElementById("app");if(el)el.textContent="BrightNet/3";});`)
	subPage := []byte(`<!DOCTYPE html><html><head><title>Sub</title></head><body><p>Subpage depth 1</p></body></html>`)
	introMedia := make([]byte, 8*1024)
	for i := range introMedia {
		introMedia[i] = byte((i*31 + 7) & 0xFF)
	}
	bigMedia := make([]byte, 40*1024)
	for i := range bigMedia {
		bigMedia[i] = byte((i*73 + 19) & 0xFF)
	}

	inputs := []InputFile{
		{RelPath: "web/index.html", Data: indexHTML},
		{RelPath: "web/assets/css/main.css", Data: mainCSS},
		{RelPath: "web/static/js/app.js", Data: appJS},
		{RelPath: "web/pages/about.html", Data: subPage},
		{RelPath: "web/media/a_intro.jpg", Data: introMedia},
		{RelPath: "web/media/z_poster.jpg", Data: bigMedia},
	}

	m, payload, err := PackFilesWithOptions(inputs, PackOptions{
		ContainerID:    "bn3-test",
		ProfileCode:    "art-fullres",
		ResolutionCode: "720p",
	})
	if err != nil {
		t.Fatalf("PackFilesWithOptions failed: %v", err)
	}

	// 1. Verify Critical-Path HTML promotion: assets/css/main.css and static/js/app.js promoted to LayerDepth == 0!
	for _, f := range m.Files {
		if f.RelPath == "web/assets/css/main.css" || f.RelPath == "web/static/js/app.js" {
			if f.LayerDepth != 0 {
				t.Fatalf("expected critical asset %s promoted to LayerDepth 0, got %d", f.RelPath, f.LayerDepth)
			}
		}
		if f.RelPath == "web/pages/about.html" && f.LayerDepth != 1 {
			t.Fatalf("expected non-critical subpage at LayerDepth 1, got %d", f.LayerDepth)
		}
	}

	// 2. Verify full WebDictV1 round-trip bit-exact
	all, err := ExtractAllToMemory(m, payload)
	if err != nil {
		t.Fatalf("ExtractAllToMemory failed: %v", err)
	}
	if !bytes.Equal(all["web/assets/css/main.css"], mainCSS) || !bytes.Equal(all["web/media/z_poster.jpg"], bigMedia) {
		t.Fatalf("WebDictV1 round-trip mismatch")
	}

	// 3. Verify HTTP/3 Zero Head-of-Line Blocking via FeedSparse:
	// Slice payload into 4096-byte frames, DROP frame 0 (which breaks contiguous prefix = 0 bytes and breaks a_intro.jpg!),
	// and feed ONLY frames >= 1 -> z_poster.jpg (starting after byte 8192) MUST unlock immediately!
	const chunkSz = 4096
	const startIdx uint32 = 2
	frames := map[uint32][]byte{}
	for fi := 0; fi*chunkSz < len(payload); fi++ {
		if fi == 0 {
			continue // Simulate dropped frame 0 -> contiguous prefix is 0 bytes!
		}
		end := (fi + 1) * chunkSz
		if end > len(payload) {
			end = len(payload)
		}
		frames[startIdx+uint32(fi)] = payload[fi*chunkSz : end]
	}

	upk, err := NewProgressiveUnpacker(m, "")
	if err != nil {
		t.Fatal(err)
	}
	defer upk.Close()

	unlocked, err := upk.FeedSparse(frames, startIdx, chunkSz)
	if err != nil {
		t.Fatalf("FeedSparse failed: %v", err)
	}
	gotBig, ok := unlocked["web/media/z_poster.jpg"]
	if !ok || !bytes.Equal(gotBig, bigMedia) {
		t.Fatalf("Zero HoL Blocking failed: z_poster.jpg should unlock even with frame 0 missing (unlocked keys=%d)", len(unlocked))
	}
	t.Logf("BrightNet/3 verified: Critical-Path depth=0, WebDictV1 payload=%d B, Zero-HoL unlocked %d file(s) with frame 0 missing!", len(payload), len(unlocked))
}

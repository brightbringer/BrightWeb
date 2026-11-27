package server

import (
	"bytes"
	"image/gif"
	"testing"
	"time"
)

func TestGeneratePreviewGIFAllCombos(t *testing.T) {
	cases := []struct {
		prof, theme, intensity string
		hideHdr                bool
	}{
		{"video-fractal", "fractal", "subtle", true},
		{"video-fractal", "fractal", "balanced", true},
		{"video-fractal", "fractal", "strong", true},
		{"video-fractal", "fractal", "max", true},
		{"video-fractal", "fractal", "balanced", false},
		{"video-fractal", "lightning", "strong", false},
		{"art-stego", "beach", "subtle", true},
		{"art-stego", "beach", "balanced", true},
		{"art-stego", "beach", "balanced", false},
		{"art-chroma", "aurora", "balanced", false},
		{"video-bw", "fractal", "subtle", true},
		{"video-bw", "fractal", "subtle", false},
		{"video-bw", "fractal", "balanced", true},
		{"video-bw", "fractal", "balanced", false},
		{"video-bw", "fractal", "strong", false},
		{"video-bw", "fractal", "max", false},
		{"art-crystal", "fractal", "balanced", true},
		{"art-crystal", "fractal", "subtle", true},
		{"art-crystal", "fractal", "balanced", false},
		{"art-fractal3d", "fractal", "balanced", true},
		{"art-angles", "aurora", "balanced", true},
		{"art-fractal-angles", "fractal", "balanced", true},
		{"art-bauhaus", "beach", "balanced", true},
		{"art-constel", "vortex", "balanced", true},
		{"art-truchet", "flora", "balanced", true},
		{"art-truchet", "flora", "subtle", false},
		{"art-fullres", "deepdream-l2", "balanced", true},
		{"art-fullres", "deepdream-l4", "balanced", true},
		{"art-fullres", "mandelbrot", "balanced", true},
		{"art-fullres", "julia-turing", "subtle", false},
		{"stealth-vhs", "none", "balanced", true},
		{"stealth-rain", "none", "balanced", true},
		{"stealth-film", "none", "balanced", true},
		{"stealth-stars", "none", "balanced", true},
		{"stealth-stars", "aurora", "balanced", true},
	}

	seen := map[string][]byte{}
	for _, tc := range cases {
		t0 := time.Now()
		raw, err := GeneratePreviewGIF(tc.prof, tc.theme, tc.intensity, tc.hideHdr)
		dt := time.Since(t0)
		if err != nil {
			t.Fatalf("GeneratePreviewGIF(%+v): %v", tc, err)
		}
		g, err := gif.DecodeAll(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("invalid GIF for %+v: %v", tc, err)
		}
		if len(g.Image) != 10 || g.Image[0].Bounds().Dx() != 320 || g.Image[0].Bounds().Dy() != 180 {
			t.Fatalf("unexpected GIF geometry: %d frames %v", len(g.Image), g.Image[0].Bounds())
		}
		k := tc.prof + "|" + tc.theme + "|" + tc.intensity + "|" + map[bool]string{true: "1", false: "0"}[tc.hideHdr]
		seen[k] = raw
		t.Logf("%-42s | %4d KB | %v", k, len(raw)/1024, dt.Round(time.Millisecond))
	}

	// Verify toggling hide_header actually changes the GIF visually
	if bytes.Equal(seen["video-fractal|fractal|balanced|1"], seen["video-fractal|fractal|balanced|0"]) {
		t.Fatal("hide_header toggle did not change video-fractal GIF!")
	}
	if bytes.Equal(seen["art-stego|beach|balanced|1"], seen["art-stego|beach|balanced|0"]) {
		t.Fatal("hide_header toggle did not change art-stego GIF!")
	}
	// Verify changing intensity actually changes the GIF visually
	if bytes.Equal(seen["video-fractal|fractal|subtle|1"], seen["video-fractal|fractal|max|1"]) {
		t.Fatal("intensity change did not change GIF!")
	}
}

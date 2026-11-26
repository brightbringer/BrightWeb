package codec

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"ytcloud/internal/media"
)

// rgbToStudioYUV converts sRGB [0..255] to studio-range BT.601 YUV clamped safely to [yLo..yHi] and [32..224].
func rgbToStudioYUV(r, g, b, yLo, yHi float64) (byte, byte, byte) {
	y := 16.0 + (65.481*r+128.553*g+24.966*b)/255.0
	cb := 128.0 + (-37.797*r-74.203*g+112.0*b)/255.0
	cr := 128.0 + (112.0*r-93.786*g-18.214*b)/255.0
	// Map Y cleanly from [16..235] into [yLo..yHi] so subtractive ±delta never clips!
	yMapped := yLo + (y-16.0)*(yHi-yLo)/219.0
	return clampYUV(int32(math.Round(yMapped)), int32(yLo), int32(yHi)),
		clampYUV(int32(math.Round(cb)), 36, 220),
		clampYUV(int32(math.Round(cr)), 36, 220)
}

// smoothPaletteConi mimics the iconic Coni63/DeepDream Mandelbrot Seahorse Valley palette
// (deep cobalt blue -> pearl white -> luminous gold -> fiery ember -> obsidian).
func smoothPaletteConi(t float64) (float64, float64, float64) {
	t = t - math.Floor(t)
	// 5-stop cyclic smooth cosine/piecewise palette
	type rgb struct{ r, g, b float64 }
	stops := []rgb{
		{8, 18, 128},    // deep sapphire blue
		{235, 240, 255}, // glowing pearl white
		{255, 198, 42},  // luminous gold
		{196, 46, 18},   // fiery ember/copper
		{24, 10, 62},    // midnight violet
		{8, 18, 128},    // wrap to sapphire
	}
	f := t * float64(len(stops)-1)
	i := int(f)
	if i >= len(stops)-1 {
		i = len(stops) - 2
	}
	u := f - float64(i)
	// Smoothstep
	u = u * u * (3.0 - 2.0*u)
	a, b := stops[i], stops[i+1]
	return a.r + (b.r-a.r)*u, a.g + (b.g-a.g)*u, a.b + (b.b-a.b)*u
}

// smoothPaletteStarry mimics Van Gogh / Coni63 DeepDream Layer 2 & 4 palette
// (midnight indigo -> cobalt -> cyan-teal -> warm cream -> starry gold).
func smoothPaletteStarry(t float64) (float64, float64, float64) {
	t = t - math.Floor(t)
	type rgb struct{ r, g, b float64 }
	stops := []rgb{
		{18, 28, 72},    // midnight blue
		{46, 98, 188},   // vibrant cobalt
		{108, 168, 212}, // luminous sky cyan
		{242, 226, 148}, // starry warm cream
		{238, 178, 38},  // van gogh gold
		{32, 74, 96},    // deep cypress teal
		{18, 28, 72},    // wrap
	}
	f := t * float64(len(stops)-1)
	i := int(f)
	if i >= len(stops)-1 {
		i = len(stops) - 2
	}
	u := f - float64(i)
	u = u * u * (3.0 - 2.0*u)
	a, b := stops[i], stops[i+1]
	return a.r + (b.r-a.r)*u, a.g + (b.g-a.g)*u, a.b + (b.b-a.b)*u
}

// smoothPalettePsyche produces vivid DeepDreamAnim / IQ-cosine iridescent colors.
func smoothPalettePsyche(t, shift float64) (float64, float64, float64) {
	p := 2.0 * math.Pi * (t + shift)
	r := 127.5 + 122.0*math.Cos(p+0.0)
	g := 127.5 + 122.0*math.Cos(p+2.094)
	b := 127.5 + 122.0*math.Cos(p+4.188)
	return r, g, b
}

type ddGenFunc func(dst *YUVFrame, frameIdx int, seed uint64, yLo, yHi float64, drawChroma bool)

func renderParallelRows(h int, fn func(y0, y1 int)) {
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers < 2 {
		fn(0, h)
		return
	}
	var wg sync.WaitGroup
	// Process in even row pairs so 4:2:0 chroma rows never race!
	pairs := h / 2
	per := (pairs + workers - 1) / workers
	for w := 0; w < workers; w++ {
		p0 := w * per
		p1 := p0 + per
		if p1 > pairs {
			p1 = pairs
		}
		if p0 >= p1 {
			break
		}
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			fn(y0, y1)
		}(p0*2, p1*2)
	}
	wg.Wait()
}

// 1. Mandelbrot Seahorse Valley Infinite Zoom (inspired by Coni63/DeepDream fractal_light!)
func renderMandelbrotSeahorse(dst *YUVFrame, frameIdx int, seed uint64, yLo, yHi float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y := dst.YPlane()
	U := dst.UPlane()
	V := dst.VPlane()
	uvW := w / 2

	// Famous Seahorse Valley spiral coordinates with continuous hypnotic zoom & gentle rotation
	cx := -0.743643887037151
	cy := 0.131825904205330
	zoom := 0.032 * math.Pow(0.935, float64(frameIdx))
	rot := float64(frameIdx) * 0.025
	cosR, sinR := math.Cos(rot), math.Sin(rot)
	aspect := float64(w) / float64(h)
	const maxIter = 96
	palShift := float64(frameIdx)*0.012 + float64(seed&0xFF)/256.0

	renderParallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			ny := (float64(y)/float64(h) - 0.5) * 2.0 * zoom
			yOff := y * w
			uvOff := (y >> 1) * uvW
			evenY := (y & 1) == 0
			for x := 0; x < w; x++ {
				nx := (float64(x)/float64(w) - 0.5) * 2.0 * zoom * aspect
				cr := cx + nx*cosR - ny*sinR
				ci := cy + nx*sinR + ny*cosR

				zr, zi := 0.0, 0.0
				iter := 0
				var zr2, zi2 float64
				for iter < maxIter {
					zr2 = zr * zr
					zi2 = zi * zi
					if zr2+zi2 > 16.0 {
						break
					}
					zi = 2.0*zr*zi + ci
					zr = zr2 - zi2 + cr
					iter++
				}

				var r, g, b float64
				if iter == maxIter {
					r, g, b = 12, 10, 26 // deep obsidian interior bulb
				} else {
					logZn := math.Log(zr2+zi2) * 0.5
					nu := float64(iter) + 1.0 - math.Log(logZn/math.Ln2)/math.Ln2
					r, g, b = smoothPaletteConi(nu*0.042 + palShift)
				}
				yv, cbv, crv := rgbToStudioYUV(r, g, b, yLo, yHi)
				Y[yOff+x] = yv
				if drawChroma && evenY && (x&1) == 0 {
					U[uvOff+(x>>1)] = cbv
					V[uvOff+(x>>1)] = crv
				}
			}
		}
	})
}

// 2. DeepDream Layer 2 — Starry Night Flowing Swirly Lines (inspired by Coni63/DeepDream Layer 2!)
func renderDeepDreamLayer2(dst *YUVFrame, frameIdx int, seed uint64, yLo, yHi float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y := dst.YPlane()
	U := dst.UPlane()
	V := dst.VPlane()
	uvW := w / 2
	aspect := float64(w) / float64(h)
	t := float64(frameIdx) * 0.06
	zoom := math.Pow(0.975, float64(frameIdx))

	// 6 drifting Van Gogh sky vortices (stream function singularities)
	type vortex struct{ x, y, strength, freq float64 }
	vortices := []vortex{
		{-0.55, -0.18, 1.35, 1.0},
		{0.12, -0.25, -1.65, 1.2},
		{0.72, -0.32, 1.45, 0.9},
		{-0.22, 0.28, -1.25, 1.1},
		{0.48, 0.24, 1.50, 1.0},
		{-0.85, 0.35, 1.10, 1.3},
	}

	renderParallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			py := (float64(y)/float64(h) - 0.5) * 2.0 * zoom
			yOff := y * w
			uvOff := (y >> 1) * uvW
			evenY := (y & 1) == 0
			for x := 0; x < w; x++ {
				px := (float64(x)/float64(w) - 0.5) * 2.0 * aspect * zoom

				// Compute fluid stream function psi(x,y) + swirling angle sum -> smooth flowing brushstrokes!
				psi := py * 1.4
				swirlGlow := 0.0
				for i, v := range vortices {
					vx := v.x + 0.14*math.Sin(t*0.7+float64(i)*1.3)
					vy := v.y + 0.12*math.Cos(t*0.6+float64(i)*1.7)
					dx := px - vx
					dy := py - vy
					r2 := dx*dx + dy*dy + 0.035
					psi += v.strength * 0.42 * math.Log(r2)
					// Add spiral twist around vortex cores
					ang := math.Atan2(dy, dx)
					psi += 0.14 * math.Sin(ang*2.0+t*0.8+float64(i)) / (1.0 + r2*3.5)
					swirlGlow += math.Exp(-r2 * 5.2)
				}

				// Layer-2 silky parallel flowing ridge lines + fine secondary wave (just like Coni63 Layer 2!)
				wave1 := math.Sin(psi*11.5 - t*2.4)
				wave2 := math.Sin((psi*23.0 + px*4.5) + t*1.5)
				ridge := 0.5 + 0.36*wave1 + 0.14*wave2

				// Color index combines vortex halos (golden stars) + cobalt sky rivers
				colT := 0.15 + 0.52*math.Min(1.0, swirlGlow*0.75) + 0.22*ridge + 0.05*math.Sin(px*1.5-t*0.4)
				r, g, b := smoothPaletteStarry(colT)
				// Emphasize brushstroke ridge shading
				shade := 0.62 + 0.55*ridge
				r = math.Min(255, r*shade)
				g = math.Min(255, g*shade)
				b = math.Min(255, b*shade)

				yv, cbv, crv := rgbToStudioYUV(r, g, b, yLo, yHi)
				Y[yOff+x] = yv
				if drawChroma && evenY && (x&1) == 0 {
					U[uvOff+(x>>1)] = cbv
					V[uvOff+(x>>1)] = crv
				}
			}
		}
	})
}

// 3. DeepDream Layer 4 — Kaleidoscopic Zoom & Psychedelic Concentric Swirl Orbs (Coni63 Layer 4 + DeepDreamAnim!)
func renderDeepDreamLayer4(dst *YUVFrame, frameIdx int, seed uint64, yLo, yHi float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y := dst.YPlane()
	U := dst.UPlane()
	V := dst.VPlane()
	uvW := w / 2
	aspect := float64(w) / float64(h)
	t := float64(frameIdx) * 0.065
	zoom := math.Pow(0.955, float64(frameIdx))

	renderParallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			py := (float64(y)/float64(h) - 0.5) * 2.0 * zoom
			yOff := y * w
			uvOff := (y >> 1) * uvW
			evenY := (y & 1) == 0
			for x := 0; x < w; x++ {
				px := (float64(x)/float64(w) - 0.5) * 2.0 * aspect * zoom

				// Iterative kaleidoscopic fold + swirl warp (3 octaves -> infinite recursive eyes & spirals!)
				ux, uy := px, py
				var eyeAcc, swirlAcc float64
				scale := 1.0
				for oct := 0; oct < 3; oct++ {
					// Smooth swirling rotation based on radius
					r := math.Hypot(ux, uy) + 1e-4
					ang := math.Atan2(uy, ux) + 0.55*math.Sin(r*3.0-t*0.9) + t*0.18*float64(oct+1)
					// 6-fold soft kaleidoscopic angular symmetry
					foldAng := math.Abs(math.Mod(ang+math.Pi, math.Pi/3.0) - math.Pi/6.0)
					ux = r * math.Cos(foldAng)
					uy = r * math.Sin(foldAng)

					// Concentric glowing "eye/orb" rings inside each folded cell
					cx := math.Mod(ux*1.8+10.0, 1.0) - 0.5
					cy := math.Mod(uy*1.8+10.0, 1.0) - 0.5
					dOrb := math.Hypot(cx, cy)
					ring := math.Cos(dOrb*18.0-t*2.5) * math.Exp(-dOrb*3.2)
					eyeAcc += ring / scale
					swirlAcc += math.Sin(r*8.0-foldAng*6.0-t*1.8) / scale

					// Zoom/fold into next fractal octave
					ux = ux*1.65 - 0.55*math.Cos(t*0.3+float64(oct))
					uy = uy*1.65 - 0.55*math.Sin(t*0.3+float64(oct))
					scale *= 1.65
				}

				val := 0.5 + 0.35*eyeAcc + 0.25*swirlAcc
				// Blend starry-night cobalt/gold with DeepDream psychedelic drift
				r1, g1, b1 := smoothPaletteStarry(val*0.85 + t*0.04)
				r2, g2, b2 := smoothPalettePsyche(val*0.55, t*0.05)
				r := 0.65*r1 + 0.35*r2
				g := 0.65*g1 + 0.35*g2
				b := 0.65*b1 + 0.35*b2
				// Crisp luminous pupil/halo contrast
				shade := math.Max(0.28, math.Min(1.25, 0.72+0.55*eyeAcc))
				r = math.Min(255, r*shade)
				g = math.Min(255, g*shade)
				b = math.Min(255, b*shade)

				yv, cbv, crv := rgbToStudioYUV(r, g, b, yLo, yHi)
				Y[yOff+x] = yv
				if drawChroma && evenY && (x&1) == 0 {
					U[uvOff+(x>>1)] = cbv
					V[uvOff+(x>>1)] = crv
				}
			}
		}
	})
}

// 4. Julia + Bio-Turing Organic Fractal (morphing Julia spirals + breathing biological Turing ridges!)
func renderJuliaTuring(dst *YUVFrame, frameIdx int, seed uint64, yLo, yHi float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y := dst.YPlane()
	U := dst.UPlane()
	V := dst.VPlane()
	uvW := w / 2
	aspect := float64(w) / float64(h)
	t := float64(frameIdx) * 0.055

	// Orbiting Douady/Spiral Julia parameter c = 0.7885 * e^(i*a)
	angC := 2.28 + 0.18*math.Sin(t*0.7)
	cRe := 0.7885 * math.Cos(angC)
	cIm := 0.7885 * math.Sin(angC)
	zoom := 1.28 * math.Pow(0.975, float64(frameIdx))
	const maxIter = 72

	renderParallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			ny := (float64(y)/float64(h) - 0.5) * 2.0 * zoom
			yOff := y * w
			uvOff := (y >> 1) * uvW
			evenY := (y & 1) == 0
			for x := 0; x < w; x++ {
				nx := (float64(x)/float64(w) - 0.5) * 2.0 * zoom * aspect
				zr, zi := nx, ny
				iter := 0
				var zr2, zi2 float64
				minTrap := 1e9
				for iter < maxIter {
					zr2 = zr * zr
					zi2 = zi * zi
					if zr2+zi2 > 16.0 {
						break
					}
					// Organic orbit trap (distance to glowing sinusoidal filament!)
					trap := math.Abs(zr*zr - zi*zi - 0.35*math.Sin(t))
					if trap < minTrap {
						minTrap = trap
					}
					zi = 2.0*zr*zi + cIm
					zr = zr2 - zi2 + cRe
					iter++
				}

				// Multi-scale Bio-Turing interference wave layered smoothly
				turing := 0.5 + 0.25*math.Sin(nx*14.0+math.Sin(ny*9.0+t)) + 0.25*math.Cos(ny*14.0-math.Cos(nx*9.0-t))
				var r, g, b float64
				if iter == maxIter {
					// Rich organic glowing interior using orbit trap + Turing ripples
					glow := math.Exp(-minTrap * 4.0)
					r, g, b = smoothPalettePsyche(0.55+glow*0.35+turing*0.18, t*0.04)
					r *= 0.35 + 0.65*glow
					g *= 0.35 + 0.65*glow
					b *= 0.35 + 0.65*glow
				} else {
					logZn := math.Log(zr2+zi2) * 0.5
					nu := float64(iter) + 1.0 - math.Log(logZn/math.Ln2)/math.Ln2
					r, g, b = smoothPalettePsyche(nu*0.038+turing*0.08, 0.12+t*0.03)
				}

				yv, cbv, crv := rgbToStudioYUV(r, g, b, yLo, yHi)
				Y[yOff+x] = yv
				if drawChroma && evenY && (x&1) == 0 {
					U[uvOff+(x>>1)] = cbv
					V[uvOff+(x>>1)] = crv
				}
			}
		}
	})
}

// embedFullResSubtractive2b adds a crisp 4-level Gray-coded offset {-36, -12, +12, +36} PER PIXEL
// over each 8x8 block WITHOUT flattening the fractal — preserving 100% 2K razor-sharp fractal spirals!
func embedFullResSubtractive2b(dst *YUVFrame, payloadChunk []byte, rsData int) {
	const margin = 16
	const bs = 8
	cols := (dst.Width - 2*margin) / bs
	rows := (dst.Height - 2*margin) / bs
	rawBytes := (cols * rows * 2) / 8
	rs := GetRSCodec(rsData)
	raw := rs.EncodeInterleaved(payloadChunk, (rawBytes/255)*255)

	Y := dst.YPlane()
	stride := dst.Width
	deltas := [4]int{-36, -12, 12, 36}
	g2e := [4]int{0, 1, 3, 2}
	bitPos := 0
	readBit := func() int {
		bIdx := bitPos >> 3
		bShift := uint(7 - (bitPos & 7))
		bitPos++
		if bIdx >= len(raw) {
			return (bitPos) & 1
		}
		return int((raw[bIdx] >> bShift) & 1)
	}

	for r := 0; r < rows; r++ {
		py := margin + r*bs
		for c := 0; c < cols; c++ {
			px := margin + c*bs
			sym := (readBit() << 1) | readBit()
			d := deltas[g2e[sym]]
			for dy := 0; dy < bs; dy++ {
				row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bs]
				for dx := 0; dx < bs; dx++ {
					v := int(row[dx]) + d
					if v < 16 {
						v = 16
					} else if v > 240 {
						v = 240
					}
					row[dx] = byte(v)
				}
			}
		}
	}
}

func decodeFullResSubtractive2b(rec *YUVFrame, origArt *YUVFrame, rsData int) ([]byte, int, error) {
	const margin = 16
	const bs = 8
	const pad = 2
	const core = 4
	cols := (rec.Width - 2*margin) / bs
	rows := (rec.Height - 2*margin) / bs
	rawBytes := (cols * rows * 2) / 8
	raw := make([]byte, rawBytes)

	rY := rec.YPlane()
	aY := origArt.YPlane()
	stride := rec.Width
	g2d := [4]int{0, 1, 3, 2}

	bitPos := 0
	putBit := func(b int) {
		bIdx := bitPos >> 3
		if bIdx < len(raw) && b != 0 {
			raw[bIdx] |= 1 << uint(7-(bitPos&7))
		}
		bitPos++
	}

	for r := 0; r < rows; r++ {
		py := margin + r*bs + pad
		for c := 0; c < cols; c++ {
			px := margin + c*bs + pad
			diffSum := 0
			for dy := 0; dy < core; dy++ {
				off := (py+dy)*stride + px
				for dx := 0; dx < core; dx++ {
					diffSum += int(rY[off+dx]) - int(aY[off+dx])
				}
			}
			avgDiff := float64(diffSum) / 16.0
			lvl := 0
			if avgDiff >= 24.0 {
				lvl = 3
			} else if avgDiff >= 0.0 {
				lvl = 2
			} else if avgDiff >= -24.0 {
				lvl = 1
			}
			g := g2d[lvl]
			putBit((g >> 1) & 1)
			putBit(g & 1)
		}
	}

	rs := GetRSCodec(rsData)
	return rs.DecodeInterleaved(raw[:(rawBytes/255)*255])
}

func TestDeepDreamFractalLab(t *testing.T) {
	if os.Getenv("YTCLOUD_DEEPDREAM_LAB") != "1" {
		t.Skip("set YTCLOUD_DEEPDREAM_LAB=1 to run the DeepDream / Mandelbrot fractal lab")
	}
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}

	const (
		W          = 2560
		H          = 1440
		FPS        = 15
		payloadLen = 128 * 1024
		rsData     = 191 // 25% RS FEC
		seed       = uint64(0xDEEFD1EA30001)
	)
	payload := make([]byte, payloadLen)
	rand.New(rand.NewSource(20261124)).Read(payload)

	cols := (W - 32) / 8
	rows := (H - 32) / 8
	rawBytes := (cols * rows * 2) / 8
	capPerFrame := (rawBytes / 255) * rsData // 10,314 B/frame -> 13 frames for 128 KB!
	nFrames := (payloadLen + capPerFrame - 1) / capPerFrame

	type ddStyle struct {
		code  string
		title string
		fn    ddGenFunc
	}
	styles := []ddStyle{
		{"mandelbrot-coni", "1. MANDELBROT SEAHORSE (CONI63)", renderMandelbrotSeahorse},
		{"deepdream-l2", "2. DEEPDREAM LAYER 2 (ONDAS STARRY)", renderDeepDreamLayer2},
		{"deepdream-l4", "3. DEEPDREAM LAYER 4 (ORBS ZOOM)", renderDeepDreamLayer4},
		{"julia-turing", "4. JULIA + BIO-TURING PSICODELICO", renderJuliaTuring},
	}

	dir := t.TempDir()

	// 2x2 high-res visual gallery PNG (2 cols x 2 rows of 640x360 full-frame downscaled views!)
	const (
		tileW = 640
		tileH = 360
		hdrH  = 34
		pad   = 14
	)
	panelW := pad + 2*(tileW+pad)
	panelH := pad + 2*(tileH+hdrH+pad)
	panel := image.NewRGBA(image.Rect(0, 0, panelW, panelH))
	for i := 0; i < len(panel.Pix); i += 4 {
		panel.Pix[i], panel.Pix[i+1], panel.Pix[i+2], panel.Pix[i+3] = 14, 16, 24, 255
	}

	t.Logf("=== DEEPDREAM & MANDELBROT FRACTAL LAB (2K @ 15fps | 128 KB | Full-Res Crisp 2-bit ±36/±12 | 13 Frm) ===")
	t.Logf("%-16s | %6s | %-18s | %-18s | %-15s | %-15s | %-15s",
		"Fractal Style", "Gen/f", "H.264 Up (CRF32)", "AV1 Up (CRF42)", "YT VP9 (fromH)", "YT VP9 (fromA)", "YT H264 (10M)")

	// Also build a 10-frame animated 2x2 GIF (480x270 total -> 240x135 per quadrant!)
	const gifFrames = 10
	gifTiles := make([][gifFrames]*image.RGBA, len(styles))

	for idx, st := range styles {
		tGen0 := time.Now()
		artFrames := make([]*YUVFrame, nFrames)
		encFrames := make([]*YUVFrame, nFrames)
		for fi := 0; fi < nFrames; fi++ {
			art := NewYUVFrame(W, H)
			st.fn(art, fi, seed, 56.0, 200.0, true)
			artFrames[fi] = art

			enc := NewYUVFrame(W, H)
			copy(enc.Buf, art.Buf)
			off := fi * capPerFrame
			end := off + capPerFrame
			if end > payloadLen {
				end = payloadLen
			}
			embedFullResSubtractive2b(enc, payload[off:end], rsData)
			encFrames[fi] = enc

			if fi < gifFrames {
				// Downsample 2560x1440 -> 240x135 for animated motion GIF
				gifTiles[idx][fi] = downsampleYUVToRGBA(enc, 240, 135)
			}
		}
		genPerFrame := time.Since(tGen0) / time.Duration(nFrames)

		// Put frame 4 (mid-zoom!) downsampled cleanly to 640x360 into the 2x2 showcase PNG!
		showFrame := encFrames[minInt(4, nFrames-1)]
		tileImg := downsampleYUVToRGBA(showFrame, tileW, tileH)
		gc, gr := idx%2, idx/2
		px0, py0 := pad+gc*(tileW+pad), pad+gr*(tileH+hdrH+pad)
		for y := 0; y < hdrH; y++ {
			for x := 0; x < tileW; x++ {
				panel.SetRGBA(px0+x, py0+y, color.RGBA{R: 26, G: 32, B: 46, A: 255})
			}
		}
		drawText(panel, px0+10, py0+10, 2, st.title, color.RGBA{R: 238, G: 244, B: 255, A: 255})
		for y := 0; y < tileH; y++ {
			for x := 0; x < tileW; x++ {
				panel.SetRGBA(px0+x, py0+hdrH+y, tileImg.RGBAAt(x, y))
			}
		}

		decodeVid := func(vidPath string) (okF, totF, corrB int, match bool) {
			totF = nFrames
			cmd := exec.Command(tools.FFmpegPath, "-loglevel", "error", "-i", vidPath, "-f", "rawvideo", "-pix_fmt", "yuv420p", "pipe:1")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				return 0, totF, 0, false
			}
			if err := cmd.Start(); err != nil {
				return 0, totF, 0, false
			}
			buf := make([]byte, W*H*3/2)
			rec := make([]byte, 0, payloadLen)
			for fi := 0; fi < nFrames; fi++ {
				if _, err := io.ReadFull(stdout, buf); err != nil {
					break
				}
				chunk, corr, err := decodeFullResSubtractive2b(&YUVFrame{Width: W, Height: H, Buf: buf}, artFrames[fi], rsData)
				if err != nil {
					continue
				}
				okF++
				corrB += corr
				off := fi * capPerFrame
				need := payloadLen - off
				if need > capPerFrame {
					need = capPerFrame
				}
				rec = append(rec, chunk[:need]...)
			}
			_ = cmd.Wait()
			return okF, totF, corrB, okF == totF && len(rec) == payloadLen && bytes.Equal(rec, payload)
		}

		h264Path := filepath.Join(dir, st.code+"_h264.mp4")
		hSize, hDur, err := runFFmpegEncodeRaw(tools.FFmpegPath, W, H, FPS, encFrames,
			[]string{"-c:v", "libx264", "-preset", "medium", "-crf", "32"}, h264Path)
		if err != nil {
			t.Fatalf("%s h264: %v", st.code, err)
		}
		hOk, hTot, hCorr, hMatch := decodeVid(h264Path)

		av1Path := filepath.Join(dir, st.code+"_av1.mp4")
		aSize, aDur, err := runFFmpegEncodeRaw(tools.FFmpegPath, W, H, FPS, encFrames,
			[]string{"-c:v", "libaom-av1", "-usage", "realtime", "-cpu-used", "8", "-crf", "42", "-b:v", "0", "-row-mt", "1", "-tiles", "2x2"}, av1Path)
		if err != nil {
			t.Fatalf("%s av1: %v", st.code, err)
		}
		aOk, aTot, aCorr, aMatch := decodeVid(av1Path)

		vp9HPath := filepath.Join(dir, st.code+"_vp9h.webm")
		vp9HSize, _ := runFFmpegTranscode(tools.FFmpegPath, h264Path, vp9HPath,
			[]string{"-c:v", "libvpx-vp9", "-deadline", "good", "-cpu-used", "5", "-b:v", "10M", "-row-mt", "1"})
		vhOk, vhTot, vhCorr, vhMatch := decodeVid(vp9HPath)

		vp9APath := filepath.Join(dir, st.code+"_vp9a.webm")
		vp9ASize, _ := runFFmpegTranscode(tools.FFmpegPath, av1Path, vp9APath,
			[]string{"-c:v", "libvpx-vp9", "-deadline", "good", "-cpu-used", "5", "-b:v", "10M", "-row-mt", "1"})
		vaOk, vaTot, vaCorr, vaMatch := decodeVid(vp9APath)

		ythPath := filepath.Join(dir, st.code+"_yth.mp4")
		ythSize, _ := runFFmpegTranscode(tools.FFmpegPath, h264Path, ythPath,
			[]string{"-c:v", "libx264", "-preset", "medium", "-b:v", "10M", "-maxrate", "12M", "-bufsize", "20M"})
		yhOk, yhTot, yhCorr, yhMatch := decodeVid(ythPath)

		fmtS := func(sz int64, ok, tot, corr int, match bool, dt time.Duration) string {
			ratio := float64(sz) / float64(payloadLen)
			mark := "OK"
			if !match {
				mark = fmt.Sprintf("FAIL(%d/%d)", ok, tot)
			} else if corr > 0 {
				mark = fmt.Sprintf("OK(%dB)", corr)
			}
			if dt > 0 {
				return fmt.Sprintf("%.2fx %.1fs %s", ratio, dt.Seconds(), mark)
			}
			return fmt.Sprintf("%.2fx %s", ratio, mark)
		}

		t.Logf("%-16s | %5dms | %-18s | %-18s | %-15s | %-15s | %-15s",
			st.code, genPerFrame.Milliseconds(),
			fmtS(hSize, hOk, hTot, hCorr, hMatch, hDur),
			fmtS(aSize, aOk, aTot, aCorr, aMatch, aDur),
			fmtS(vp9HSize, vhOk, vhTot, vhCorr, vhMatch, 0),
			fmtS(vp9ASize, vaOk, vaTot, vaCorr, vaMatch, 0),
			fmtS(ythSize, yhOk, yhTot, yhCorr, yhMatch, 0),
		)
	}

	if outPNG := os.Getenv("YTCLOUD_DD_PNG"); outPNG != "" {
		f, err := os.Create(outPNG)
		if err != nil {
			t.Fatalf("create DD PNG: %v", err)
		}
		_ = png.Encode(f, panel)
		_ = f.Close()
		t.Logf("Saved DeepDream PNG gallery: %s (%dx%d)", outPNG, panelW, panelH)
	}

	if outGIF := os.Getenv("YTCLOUD_DD_GIF"); outGIF != "" {
		// 216-color web-safe cube + 32 gray palette
		pal := make(color.Palette, 0, 248)
		for r := 0; r < 6; r++ {
			for g := 0; g < 6; g++ {
				for b := 0; b < 6; b++ {
					pal = append(pal, color.RGBA{uint8(r * 51), uint8(g * 51), uint8(b * 51), 255})
				}
			}
		}
		for i := 0; i < 32; i++ {
			v := uint8(i * 255 / 31)
			pal = append(pal, color.RGBA{v, v, v, 255})
		}
		outG := &gif.GIF{
			Image: make([]*image.Paletted, gifFrames),
			Delay: make([]int, gifFrames),
		}
		const gw, gh = 488, 278 // 2x240 + 8px gap, 2x135 + 8px gap
		for fi := 0; fi < gifFrames; fi++ {
			pImg := image.NewPaletted(image.Rect(0, 0, gw, gh), pal)
			for idx := 0; idx < 4; idx++ {
				src := gifTiles[idx][fi]
				ox := (idx % 2) * 244
				oy := (idx / 2) * 139
				for y := 0; y < 135; y++ {
					for x := 0; x < 240; x++ {
						c := src.RGBAAt(x, y)
						ri := (int(c.R)*5 + 127) / 255
						gi := (int(c.G)*5 + 127) / 255
						bi := (int(c.B)*5 + 127) / 255
						pImg.SetColorIndex(ox+x, oy+y, uint8(ri*36+gi*6+bi))
					}
				}
			}
			outG.Image[fi] = pImg
			outG.Delay[fi] = 12
		}
		f, err := os.Create(outGIF)
		if err != nil {
			t.Fatalf("create DD GIF: %v", err)
		}
		_ = gif.EncodeAll(f, outG)
		_ = f.Close()
		t.Logf("Saved DeepDream animated GIF: %s", outGIF)
	}
}

func downsampleYUVToRGBA(f *YUVFrame, dw, dh int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, dw, dh))
	Y := f.YPlane()
	U := f.UPlane()
	V := f.VPlane()
	w, h := f.Width, f.Height
	uvW := w / 2
	for y := 0; y < dh; y++ {
		sy := (y * h) / dh
		yOff := sy * w
		uvOff := (sy >> 1) * uvW
		for x := 0; x < dw; x++ {
			sx := (x * w) / dw
			yv := float64(Y[yOff+sx])
			cb := float64(U[uvOff+(sx>>1)]) - 128.0
			cr := float64(V[uvOff+(sx>>1)]) - 128.0
			r := yv + 1.402*cr
			g := yv - 0.344136*cb - 0.714136*cr
			b := yv + 1.772*cb
			clampB := func(v float64) uint8 {
				if v < 0 {
					return 0
				}
				if v > 255 {
					return 255
				}
				return uint8(math.Round(v))
			}
			img.SetRGBA(x, y, color.RGBA{clampB(r), clampB(g), clampB(b), 255})
		}
	}
	return img
}

func TestJuliaInfiniteZoomVisualStrip(t *testing.T) {
	if os.Getenv("YTCLOUD_JULIA_VERIFY") != "1" {
		t.Skip("set YTCLOUD_JULIA_VERIFY=1")
	}
	const w, h = 480, 270
	out := image.NewRGBA(image.Rect(0, 0, w*2+12, h*2+12))

	// Render actual production renderThemeJuliaTuring across 1 full seamless 38-frame ultra-macro cycle:
	// TL: frame 0, TR: frame 13, BL: frame 25, BR: frame 37 (just before seamless wrap back to frame 0!)
	frameIdxs := []int{0, 13, 25, 37}
	for i, fi := range frameIdxs {
		f := NewYUVFrame(w, h)
		renderThemeJuliaTuring(f, fi, 0xF1AC7A13, 44, 212, true)
		rgba := downsampleYUVToRGBA(f, w, h)
		ox := (i % 2) * (w + 12)
		oy := (i / 2) * (h + 12)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.SetRGBA(ox+x, oy+y, rgba.RGBAAt(x, y))
			}
		}
	}
	p := os.Getenv("YTCLOUD_JULIA_PNG")
	fp, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = png.Encode(fp, out)
	_ = fp.Close()
}

func TestRender15sFractalShowcaseVideos(t *testing.T) {
	outDir := os.Getenv("YTCLOUD_SHOWCASE_DIR")
	if outDir == "" {
		t.Skip("set YTCLOUD_SHOWCASE_DIR to render 15s fractal showcase MP4s")
	}
	onlyFile := os.Getenv("YTCLOUD_SHOWCASE_ONLY")
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := []byte(`{"showcase":"ytcloud-15s-fractal-preview"}`)
	payload := make([]byte, 32*1024)
	rand.New(rand.NewSource(20261124)).Read(payload)

	items := []struct {
		file  string
		theme uint8
	}{
		{"01_mandelbrot_seahorse_15s.mp4", ArtThemeMandelbrot},
		{"02_deepdream_layer2_starry_15s.mp4", ArtThemeDeepDreamL2},
		{"03_deepdream_layer4_orbs_15s.mp4", ArtThemeDeepDreamL4},
		{"04_julia_bioturing_zoom_15s.mp4", ArtThemeJuliaTuring},
	}

	for i, it := range items {
		if onlyFile != "" && it.file != onlyFile {
			continue
		}
		t0 := time.Now()
		outPath := filepath.Join(outDir, it.file)
		stats, err := EncodeStreamToVideo(VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outPath,
			ResolutionCode: "720p",
			ProfileCode:    "art-fullres",
			VideoCodec:     "h264-mp4",
			AudioMode:      "none",
			FPS:            15,
			Threads:        4,
			LowPriority:    false,
			ContainerID:    0xF1AC7A10 + uint64(i),
			ArtTheme:       it.theme,
			HideHeaderBar:  true,
			TargetFrames:   225, // 15.0 seconds @ 15 FPS
			PaddingMode:    "auto",
		}, manifest, payload)
		if err != nil {
			t.Fatalf("%s encode failed: %v", it.file, err)
		}
		t.Logf("%-36s | %.2f MB | rendered in %v", it.file, float64(stats.VideoSizeBytes)/(1024*1024), time.Since(t0).Round(100*time.Millisecond))
	}
}

func TestSeamlessEdgeToEdgeHiddenHeaderAllModes(t *testing.T) {
	const w, h = 1280, 720
	host := NewYUVFrame(w, h)
	// Synthetic host video frame (smooth diagonal gradient + circle silhouette + colored chroma)
	hY, hU, hV := host.YPlane(), host.UPlane(), host.VPlane()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 40 + (x*140)/w + (y*40)/h
			if (x-w/2)*(x-w/2)+(y-h/2)*(y-h/2) < 180*180 {
				v = 210
			}
			if v > 235 {
				v = 235
			}
			hY[y*w+x] = byte(v)
		}
	}
	for cy := 0; cy < h/2; cy++ {
		for cx := 0; cx < w/2; cx++ {
			hU[cy*(w/2)+cx] = byte(80 + (cx*96)/(w/2))
			hV[cy*(w/2)+cx] = byte(80 + (cy*96)/(h/2))
		}
	}

	cases := []struct {
		code    string
		theme   uint8
		useHost bool
		mi      uint8
	}{
		{"video-fractal", ArtThemeJuliaTuring, true, MaskIntensityDefault},
		{"video-fractal", ArtThemeNone, true, MaskIntensitySubtle},
		{"video-chroma", ArtThemeNone, true, MaskIntensityDefault},
		{"art-fractal-angles", ArtThemeNone, true, MaskIntensityDefault},
		{"art-crystal", ArtThemeDeepDreamL2, false, MaskIntensityDefault},
		{"art-bauhaus", ArtThemeNone, false, MaskIntensityDefault},
		{"art-fullres", ArtThemeJuliaTuring, false, MaskIntensityDefault},
		{"art-fractal3d", ArtThemeDeepDreamL2, false, MaskIntensityDefault},
		{"art-stego", ArtThemeNone, true, MaskIntensityDefault},
		{"video-bw", ArtThemeNone, true, MaskIntensityDefault},
	}

	var visFrames []*YUVFrame
	for i, tc := range cases {
		prof, err := ProfileByCode(tc.code)
		if err != nil {
			t.Fatalf("profile %s: %v", tc.code, err)
		}
		flags := BuildHeaderFlags(tc.mi, true)
		if tc.useHost && (prof.ArtMode == "subtractive-y2" || prof.ArtMode == "video-bw") {
			flags |= FlagHostManchester
		}
		capB := prof.PayloadCapacityLayoutWithFlags(w, h, true, flags)
		payload := make([]byte, capB)
		rand.New(rand.NewSource(int64(7000 + i))).Read(payload)

		dst := NewYUVFrame(w, h)
		hdr := FrameHeader{
			ContainerID: 0xABCD12340000 + uint64(i),
			ArtTheme:    tc.theme,
			Flags:       flags,
			FrameIndex:  3,
			TotalFrames: 10,
		}
		var hPtr *YUVFrame
		if tc.useHost {
			hPtr = host
		}
		if err := EncodeFrameWithHost(dst, hPtr, prof, hdr, payload); err != nil {
			t.Fatalf("%s encode failed: %v", tc.code, err)
		}
		dec, err := DecodeFrame(dst)
		if err != nil {
			t.Fatalf("%s (theme=%d host=%v mi=%d) decode failed: %v", tc.code, tc.theme, tc.useHost, tc.mi, err)
		}
		if !bytes.Equal(dec.Payload, payload) {
			t.Fatalf("%s payload mismatch (corrected=%d)", tc.code, dec.CorrectedBytes)
		}
		if i == 0 || i == 2 || i == 3 || i == 4 {
			visFrames = append(visFrames, dst)
		}
	}

	if outPNG := os.Getenv("YTCLOUD_EDGE_PNG"); outPNG != "" && len(visFrames) == 4 {
		const tw, th = 480, 270
		out := image.NewRGBA(image.Rect(0, 0, tw*2+12, th*2+12))
		for i, f := range visFrames {
			rgba := downsampleYUVToRGBA(f, tw, th)
			ox := (i % 2) * (tw + 12)
			oy := (i / 2) * (th + 12)
			for y := 0; y < th; y++ {
				for x := 0; x < tw; x++ {
					out.SetRGBA(ox+x, oy+y, rgba.RGBAAt(x, y))
				}
			}
		}
		fp, err := os.Create(outPNG)
		if err == nil {
			_ = png.Encode(fp, out)
			_ = fp.Close()
		}
	}
}

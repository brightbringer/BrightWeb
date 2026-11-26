package codec

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"strings"
)

const (
	ArtThemeAuto      uint8 = 0 // Automático (escolhido pela semente/palavra-chave)
	ArtThemeAurora    uint8 = 1 // Aurora & Plasma
	ArtThemeWinamp    uint8 = 2 // Ondas Musicais (Osciloscópio + Espectro)
	ArtThemeLightning uint8 = 3 // Raios & Energia (Arcos Neon)
	ArtThemeVortex    uint8 = 4 // Túnel Hipnótico (Espiral Radial)
	ArtThemeBeach     uint8 = 5 // Praia & Oceano (Pôr-do-sol + Ondas do Mar)
	ArtThemeFlora       uint8 = 6  // Flora & Natureza (Padrões Orgânicos / Botânicos)
	ArtThemeFractal     uint8 = 7  // Sonho Fractal (Domain-Warped fBm / Aquarela Líquida)
	ArtThemeMandelbrot  uint8 = 8  // Mandelbrot Seahorse Zoom (Coni63 / Espirais Douradas)
	ArtThemeDeepDreamL2 uint8 = 9  // DeepDream Layer 2 (Ondas Starry Night)
	ArtThemeDeepDreamL4 uint8 = 10 // DeepDream Layer 4 (Orbs & Caleidoscópio Zoom)
	ArtThemeJuliaTuring uint8 = 11 // Julia + Bio-Turing Psicodélico
	ArtThemeNone        uint8 = 12 // Nenhum (Sem Efeito Artístico — vídeo base original ou neutro)
)

type ArtThemeInfo struct {
	ID   uint8  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

func ListArtThemes() []ArtThemeInfo {
	return []ArtThemeInfo{
		{ID: ArtThemeNone, Code: "none", Name: "🚫 Nenhum (Sem Efeito Artístico)"},
		{ID: ArtThemeDeepDreamL2, Code: "deepdream-l2", Name: "🎨 DeepDream Layer 2 (Ondas Starry Night · ~6,9×)"},
		{ID: ArtThemeDeepDreamL4, Code: "deepdream-l4", Name: "👁️ DeepDream Layer 4 (Orbs & Caleidoscópio Zoom · ~8,0×)"},
		{ID: ArtThemeMandelbrot, Code: "mandelbrot", Name: "🌌 Mandelbrot Seahorse Zoom (Coni63 · Espirais Douradas)"},
		{ID: ArtThemeJuliaTuring, Code: "julia-turing", Name: "🧬 Julia + Bio-Turing Psicodélico"},
		{ID: ArtThemeFractal, Code: "fractal", Name: "☁️ Sonho Fractal (Nuvens & Aquarela)"},
		{ID: ArtThemeAuto, Code: "auto", Name: "🎲 Automático (Muda a cada vídeo)"},
		{ID: ArtThemeWinamp, Code: "waves", Name: "🎵 Ondas Musicais (Osciloscópio & Espectro)"},
		{ID: ArtThemeLightning, Code: "lightning", Name: "⚡ Raios & Energia (Arcos Neon)"},
		{ID: ArtThemeVortex, Code: "vortex", Name: "🌀 Túnel Hipnótico (Espiral Radial)"},
		{ID: ArtThemeBeach, Code: "beach", Name: "🏖️ Praia & Oceano (Ondas & Pôr-do-Sol)"},
		{ID: ArtThemeFlora, Code: "flora", Name: "🌿 Flora & Natureza (Folhas & Espirais)"},
		{ID: ArtThemeAurora, Code: "aurora", Name: "🌌 Aurora & Plasma"},
	}
}

// ResolveArtTheme maps a theme code and/or free-form user keyword (e.g. "praia", "cachorro", "raios", "fractal")
// into a concrete ArtTheme ID and a deterministic seed mix so every keyword creates a distinct palette & melody!
func ResolveArtTheme(themeCode, customKeyword string, baseSeed uint64) (uint8, uint64) {
	kw := strings.ToLower(strings.TrimSpace(customKeyword))
	seed := baseSeed
	if kw != "" {
		h := fnv.New64a()
		_, _ = h.Write([]byte("ytcloud-theme:" + kw))
		seed ^= h.Sum64()
	}

	switch strings.ToLower(strings.TrimSpace(themeCode)) {
	case "none", "nenhum", "off":
		return ArtThemeNone, seed
	case "aurora":
		return ArtThemeAurora, seed
	case "waves", "winamp", "music", "musica", "música":
		return ArtThemeWinamp, seed
	case "lightning", "raios", "raio", "energia":
		return ArtThemeLightning, seed
	case "vortex", "tunel", "túnel", "milkdrop":
		return ArtThemeVortex, seed
	case "beach", "praia", "mar", "oceano":
		return ArtThemeBeach, seed
	case "flora", "natureza", "floresta", "planta":
		return ArtThemeFlora, seed
	case "fractal", "sonho", "sonhos", "dream", "nuvem", "nuvens", "aquarela":
		return ArtThemeFractal, seed
	case "mandelbrot", "seahorse", "coni", "coni63":
		return ArtThemeMandelbrot, seed
	case "deepdream-l2", "deepdream2", "starry", "vangogh", "pincelada":
		return ArtThemeDeepDreamL2, seed
	case "deepdream-l4", "deepdream4", "deepdream", "orbs", "caleidoscopio", "olhos":
		return ArtThemeDeepDreamL4, seed
	case "julia-turing", "julia", "turing", "bioturing", "bio":
		return ArtThemeJuliaTuring, seed
	}

	// Keyword heuristic when themeCode is "auto" or empty
	if kw != "" {
		switch {
		case strings.Contains(kw, "mandelbrot") || strings.Contains(kw, "seahorse") || strings.Contains(kw, "coni"):
			return ArtThemeMandelbrot, seed
		case strings.Contains(kw, "starry") || strings.Contains(kw, "vangogh") || strings.Contains(kw, "layer 2") || strings.Contains(kw, "layer2"):
			return ArtThemeDeepDreamL2, seed
		case strings.Contains(kw, "deepdream") || strings.Contains(kw, "caleidosc") || strings.Contains(kw, "olho") || strings.Contains(kw, "orbs"):
			return ArtThemeDeepDreamL4, seed
		case strings.Contains(kw, "julia") || strings.Contains(kw, "turing") || strings.Contains(kw, "coral"):
			return ArtThemeJuliaTuring, seed
		case strings.Contains(kw, "fractal") || strings.Contains(kw, "sonho") || strings.Contains(kw, "dream") || strings.Contains(kw, "nuvem") || strings.Contains(kw, "nuvens") || strings.Contains(kw, "aquarela") || strings.Contains(kw, "psicodel"):
			return ArtThemeFractal, seed
		case strings.Contains(kw, "praia") || strings.Contains(kw, "mar") || strings.Contains(kw, "oceano") || strings.Contains(kw, "sol") || strings.Contains(kw, "agua") || strings.Contains(kw, "água"):
			return ArtThemeBeach, seed
		case strings.Contains(kw, "flora") || strings.Contains(kw, "flor") || strings.Contains(kw, "mata") || strings.Contains(kw, "floresta") || strings.Contains(kw, "planta") || strings.Contains(kw, "fauna") || strings.Contains(kw, "gato") || strings.Contains(kw, "cachorro") || strings.Contains(kw, "animal"):
			return ArtThemeFlora, seed
		case strings.Contains(kw, "raio") || strings.Contains(kw, "trovao") || strings.Contains(kw, "trovão") || strings.Contains(kw, "energia") || strings.Contains(kw, "eletric") || strings.Contains(kw, "neon"):
			return ArtThemeLightning, seed
		case strings.Contains(kw, "winamp") || strings.Contains(kw, "music") || strings.Contains(kw, "músic") || strings.Contains(kw, "som") || strings.Contains(kw, "onda") || strings.Contains(kw, "beat"):
			return ArtThemeWinamp, seed
		case strings.Contains(kw, "tunel") || strings.Contains(kw, "túnel") || strings.Contains(kw, "espiral") || strings.Contains(kw, "galax") || strings.Contains(kw, "espaco") || strings.Contains(kw, "espaço"):
			return ArtThemeVortex, seed
		}
	}

	// Deterministic pick across all 11 themes from seed
	return uint8(1 + (seed % 11)), seed
}

const sinLUTSize = 4096
const sinLUTMask = sinLUTSize - 1

var sinLUT [sinLUTSize]float64

func init() {
	for i := 0; i < sinLUTSize; i++ {
		sinLUT[i] = math.Sin(float64(i) * (2.0 * math.Pi / float64(sinLUTSize)))
	}
}

func fastSin(cycles float64) float64 {
	idx := int(cycles*float64(sinLUTSize)) & sinLUTMask
	return sinLUT[idx]
}

func clampYUV(v, lo, hi int32) byte {
	if v < lo {
		return byte(lo)
	}
	if v > hi {
		return byte(hi)
	}
	return byte(v)
}

// RenderProceduralArt draws a deterministic themed Winamp/MilkDrop procedural frame into YUVFrame.
// Because it depends strictly on (theme, frameIdx, seed), the decoder regenerates the exact same
// carrier A(x,y) and subtracts it out (Subtractive Carrier Cancellation) with zero drift!
func RenderProceduralArt(dst *YUVFrame, frameIdx int, seed uint64, theme uint8, yMin, yMax float64, drawChroma bool) {
	if theme == ArtThemeNone {
		if yMax > yMin {
			mid := clampYUV(int32(math.Round((yMin+yMax)*0.5)), 16, 235)
			Y := dst.YPlane()
			for i := range Y {
				Y[i] = mid
			}
		}
		fillNeutralUV(dst.UPlane(), dst.VPlane())
		return
	}
	if theme == ArtThemeAuto || theme > ArtThemeJuliaTuring {
		theme = uint8(1 + (seed % 11))
	}

	switch theme {
	case ArtThemeWinamp:
		renderWinampWaves(dst, frameIdx, seed, yMin, yMax, drawChroma)
	case ArtThemeLightning:
		renderTeslaLightning(dst, frameIdx, seed, yMin, yMax, drawChroma)
	case ArtThemeVortex:
		renderMilkdropVortex(dst, frameIdx, seed, yMin, yMax, drawChroma)
	case ArtThemeBeach:
		renderOceanBeach(dst, frameIdx, seed, yMin, yMax, drawChroma)
	case ArtThemeFlora:
		renderFloraOrganic(dst, frameIdx, seed, yMin, yMax, drawChroma)
	case ArtThemeFractal:
		renderFractalDream(dst, frameIdx, seed, yMin, yMax, drawChroma)
	case ArtThemeMandelbrot:
		renderThemeMandelbrot(dst, frameIdx, seed, yMin, yMax, drawChroma)
	case ArtThemeDeepDreamL2:
		renderThemeDeepDreamL2(dst, frameIdx, seed, yMin, yMax, drawChroma)
	case ArtThemeDeepDreamL4:
		renderThemeDeepDreamL4(dst, frameIdx, seed, yMin, yMax, drawChroma)
	case ArtThemeJuliaTuring:
		renderThemeJuliaTuring(dst, frameIdx, seed, yMin, yMax, drawChroma)
	default:
		renderAuroraPlasma(dst, frameIdx, seed, yMin, yMax, drawChroma)
	}
}

// Theme 1: Aurora & Plasma (smooth harmonic interference)
func renderAuroraPlasma(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW, uvH := w/2, h/2

	t := float64(frameIdx) * 0.18
	p1 := float64(seed&0xFF) * 0.0245
	p2 := float64((seed>>8)&0xFF) * 0.0245
	p3 := float64((seed>>16)&0xFF) * 0.0245

	freqX1 := 1.5 + float64((seed>>24)&3)*0.5
	freqX2 := 2.5 + float64((seed>>26)&3)*0.6
	freqY1 := 1.5 + float64((seed>>28)&3)*0.5
	freqY2 := 2.2 + float64((seed>>30)&3)*0.6

	yMid := int32(math.Round((yMin + yMax) * 0.5))
	yHalf := (yMax - yMin) * 0.5

	if yHalf > 0 {
		const fp = 256.0
		sinX1 := make([]int32, w)
		cosX2 := make([]int32, w)
		sinX3 := make([]int32, w)
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(w)
			sinX1[x] = int32(math.Round(math.Sin(fx*2*math.Pi*freqX1+t+p1) * fp))
			cosX2[x] = int32(math.Round(math.Cos(fx*2*math.Pi*freqX2-t*0.65+p2) * fp))
			sinX3[x] = int32(math.Round(math.Sin(fx*2*math.Pi*(freqX1+1.0)+t*0.4+p3) * (fp * 0.5)))
		}
		scaleY := (yHalf / 2.5) * fp
		sinY1 := make([]int32, h)
		cosY2 := make([]int32, h)
		cosY3 := make([]int32, h)
		for y := 0; y < h; y++ {
			fy := float64(y) / float64(h)
			sinY1[y] = int32(math.Round(math.Sin(fy*2*math.Pi*freqY1-t*0.75+p2) * scaleY))
			cosY2[y] = int32(math.Round(math.Cos(fy*2*math.Pi*freqY2+t*0.55+p1) * scaleY))
			cosY3[y] = int32(math.Round(math.Cos(fy*2*math.Pi*(freqY1+0.8)-t*0.35+p3) * scaleY))
		}
		for y := 0; y < h; y++ {
			sy1, cy2, cy3 := sinY1[y], cosY2[y], cosY3[y]
			row := Y[y*w : (y+1)*w]
			for x := 0; x < w; x++ {
				row[x] = clampYUV(yMid+((sinX1[x]*cy2+cosX2[x]*sy1+sinX3[x]*cy3)>>16), 16, 235)
			}
		}
	}

	if drawChroma {
		uBias := int32(math.Round(128.0 + float64(int((seed>>32)&31)-16)*0.9))
		vBias := int32(math.Round(128.0 + float64(int((seed>>37)&31)-16)*0.9))
		const cScale = 52.0 * 256.0
		uX := make([]int32, uvW)
		vX := make([]int32, uvW)
		for cx := 0; cx < uvW; cx++ {
			fx := float64(cx) / float64(uvW)
			uX[cx] = int32(math.Round(math.Sin(fx*2*math.Pi*1.6+t*0.52+p2) * 256.0))
			vX[cx] = int32(math.Round(math.Cos(fx*2*math.Pi*1.9-t*0.44+p1) * 256.0))
		}
		for cy := 0; cy < uvH; cy++ {
			fy := float64(cy) / float64(uvH)
			sy := int32(math.Round(math.Sin(fy*2*math.Pi*1.4+t*0.45+p1) * cScale))
			cy2 := int32(math.Round(math.Cos(fy*2*math.Pi*2.1-t*0.38+p3) * cScale))
			uRow := U[cy*uvW : (cy+1)*uvW]
			vRow := V[cy*uvW : (cy+1)*uvW]
			for cx := 0; cx < uvW; cx++ {
				uRow[cx] = clampYUV(uBias+((uX[cx]*cy2)>>16), 24, 232)
				vRow[cx] = clampYUV(vBias+((vX[cx]*sy)>>16), 24, 232)
			}
		}
	} else {
		fillNeutralUV(U, V)
	}
}

// Theme 2: Winamp Musical Waves (audio-synced glowing oscilloscope + smooth spectrum visualizer!)
func renderWinampWaves(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW, uvH := w/2, h/2

	// Sync exact musical pulse to SynthAmbientMusicWAV (10 FPS -> t = frameIdx * 0.1s, bellStep = 0.5s)
	tSec := float64(frameIdx) * 0.1
	pulse := 0.65 + 0.35*math.Abs(math.Sin(tSec*math.Pi*2.0)) // beats every 0.5s with bell notes!
	bar := int(tSec/4.0) & 3
	p1 := float64(seed&0xFF) * 0.025

	// Precompute 3 layered oscilloscope wave centerlines Y(x) in [0..1]
	wave1 := make([]float64, w)
	wave2 := make([]float64, w)
	specH := make([]float64, w)
	for x := 0; x < w; x++ {
		fx := float64(x) / float64(w)
		// Main glowing Winamp oscilloscope ribbon
		w1 := 0.48 + 0.18*pulse*math.Sin(fx*2*math.Pi*3.0+tSec*3.2+p1) + 0.08*math.Sin(fx*2*math.Pi*7.0-tSec*5.1)
		w2 := 0.52 + 0.15*pulse*math.Cos(fx*2*math.Pi*2.0-tSec*2.5+float64(bar)) + 0.07*math.Sin(fx*2*math.Pi*5.0+tSec*4.0)
		wave1[x] = w1
		wave2[x] = w2
		// Smooth spectrum equalizer hills at bottom (24 musical bands)
		band := int(fx * 24.0)
		bandPhase := float64((seed>>uint((band%8)*4))&15)*0.4 + float64(band)*0.55
		specH[x] = 0.12 + 0.22*pulse*(0.5+0.5*math.Sin(tSec*3.8+bandPhase))
	}

	ySpan := yMax - yMin
	if ySpan > 0 {
		for y := 0; y < h; y++ {
			fy := float64(y) / float64(h)
			row := Y[y*w : (y+1)*w]
			invFY := 1.0 - fy
			for x := 0; x < w; x++ {
				d1 := math.Abs(fy - wave1[x])
				d2 := math.Abs(fy - wave2[x])
				// Soft Gaussian-like glow (smooth enough for H.264/VP9, unmistakable Winamp waveform!)
				glow := math.Exp(-d1*16.0)*0.75 + math.Exp(-d2*14.0)*0.55
				if invFY < specH[x] {
					glow += 0.45 * (1.0 - invFY/specH[x])
				}
				if glow > 1.0 {
					glow = 1.0
				}
				row[x] = clampYUV(int32(yMin+glow*ySpan), 16, 235)
			}
		}
	}

	if drawChroma {
		// Neon Winamp cyan -> magenta -> lime palette shifting with chord bars
		shift := float64(bar)*0.8 + tSec*0.35
		for cy := 0; cy < uvH; cy++ {
			fy := float64(cy) / float64(uvH)
			uRow := U[cy*uvW : (cy+1)*uvW]
			vRow := V[cy*uvW : (cy+1)*uvW]
			for cx := 0; cx < uvW; cx++ {
				fx := float64(cx) / float64(uvW)
				d := math.Abs(fy - wave1[cx*2])
				if d < 0.08 {
					// Bright cyan/electric blue wave core
					uRow[cx] = clampYUV(int32(195+25*math.Sin(fx*6.0+shift)), 24, 232)
					vRow[cx] = clampYUV(int32(78+45*math.Cos(fx*4.0-shift)), 24, 232)
				} else {
					// Deep purple/magenta Winamp background glow
					uRow[cx] = clampYUV(int32(148+38*math.Sin(fx*4.0+fy*3.0+shift)), 24, 232)
					vRow[cx] = clampYUV(int32(152+42*math.Cos(fx*3.0-fy*4.0+shift)), 24, 232)
				}
			}
		}
	} else {
		fillNeutralUV(U, V)
	}
}

// Theme 3: Tesla Lightning & Energy Arcs (electric neon filaments across dark void)
func renderTeslaLightning(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW, uvH := w/2, h/2

	t := float64(frameIdx) * 0.25
	p1 := float64(seed&0xFF) * 0.03
	p2 := float64((seed>>8)&0xFF) * 0.03

	bolt1 := make([]float64, w)
	bolt2 := make([]float64, w)
	bolt3 := make([]float64, w)
	for x := 0; x < w; x++ {
		fx := float64(x) / float64(w)
		// Jagged harmonic lightning paths
		bolt1[x] = 0.30 + 0.14*math.Sin(fx*9.0+t*2.1+p1) + 0.06*math.Sin(fx*23.0-t*3.7) + 0.03*math.Cos(fx*47.0+t*5.0)
		bolt2[x] = 0.52 + 0.16*math.Cos(fx*7.5-t*1.9+p2) + 0.07*math.Sin(fx*19.0+t*4.1) + 0.03*math.Sin(fx*41.0-t*6.0)
		bolt3[x] = 0.74 + 0.12*math.Sin(fx*11.0+t*2.6+p1+p2) + 0.05*math.Cos(fx*29.0-t*3.3)
	}

	ySpan := yMax - yMin
	if ySpan > 0 {
		for y := 0; y < h; y++ {
			fy := float64(y) / float64(h)
			row := Y[y*w : (y+1)*w]
			for x := 0; x < w; x++ {
				d := math.Abs(fy - bolt1[x])
				if d2 := math.Abs(fy - bolt2[x]); d2 < d {
					d = d2
				}
				if d3 := math.Abs(fy - bolt3[x]); d3 < d {
					d = d3
				}
				energy := math.Exp(-d * 22.0)
				// Subtle ambient storm glow
				glow := 0.12 + 0.88*energy
				row[x] = clampYUV(int32(yMin+glow*ySpan), 16, 235)
			}
		}
	}

	if drawChroma {
		for cy := 0; cy < uvH; cy++ {
			fy := float64(cy) / float64(uvH)
			uRow := U[cy*uvW : (cy+1)*uvW]
			vRow := V[cy*uvW : (cy+1)*uvW]
			for cx := 0; cx < uvW; cx++ {
				d := math.Abs(fy - bolt2[cx*2])
				if d1 := math.Abs(fy - bolt1[cx*2]); d1 < d {
					d = d1
				}
				if d3 := math.Abs(fy - bolt3[cx*2]); d3 < d {
					d = d3
				}
				e := math.Exp(-d * 14.0)
				// Electric cyan bolts on deep violet-indigo storm sky
				uRow[cx] = clampYUV(int32(168+52*e), 24, 232)
				vRow[cx] = clampYUV(int32(158-68*e), 24, 232)
			}
		}
	} else {
		fillNeutralUV(U, V)
	}
}

// Theme 4: MilkDrop Hypnotic Vortex Tunnel (concentric rotating spirals)
func renderMilkdropVortex(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW, uvH := w/2, h/2

	t := float64(frameIdx) * 0.22
	arms := float64(3 + int(seed%4)) // 3..6 spiral arms
	cx0 := float64(w) * (0.5 + 0.08*math.Sin(t*0.3))
	cy0 := float64(h) * (0.5 + 0.08*math.Cos(t*0.25))
	invR := 1.0 / (float64(h) * 0.5)

	yMid := (yMin + yMax) * 0.5
	yHalf := (yMax - yMin) * 0.5

	// Render at 4x4 grid smoothness for blazing 4K speed (~4ms!) while staying mathematically exact
	if yHalf > 0 {
		for y := 0; y < h; y += 2 {
			dy := (float64(y) - cy0) * invR
			dy2 := dy * dy
			row0 := Y[y*w : (y+1)*w]
			row1 := Y[(y+1)*w : (y+2)*w]
			for x := 0; x < w; x += 2 {
				dx := (float64(x) - cx0) * invR
				r := math.Sqrt(dx*dx + dy2 + 0.002)
				ang := math.Atan2(dy, dx)
				val := 0.55*fastSin((2.2/r-t*0.35)/(2*math.Pi)*2*math.Pi) + 0.45*fastSin((arms*ang+5.0*r-t)/(2*math.Pi))
				b := clampYUV(int32(yMid+yHalf*val), 16, 235)
				row0[x], row0[x+1] = b, b
				row1[x], row1[x+1] = b, b
			}
		}
	}

	if drawChroma {
		for cy := 0; cy < uvH; cy++ {
			dy := (float64(cy*2) - cy0) * invR
			dy2 := dy * dy
			uRow := U[cy*uvW : (cy+1)*uvW]
			vRow := V[cy*uvW : (cy+1)*uvW]
			for cx := 0; cx < uvW; cx++ {
				dx := (float64(cx*2) - cx0) * invR
				r := math.Sqrt(dx*dx + dy2 + 0.002)
				ang := math.Atan2(dy, dx)
				uRow[cx] = clampYUV(int32(128.0+58.0*fastSin((ang*2.0+r*3.0-t*0.8)/(2*math.Pi))), 24, 232)
				vRow[cx] = clampYUV(int32(128.0+58.0*fastSin((ang*2.0-r*3.5+t*0.6+1.5)/(2*math.Pi))), 24, 232)
			}
		}
	} else {
		fillNeutralUV(U, V)
	}
}

// Theme 5: Beach & Ocean (golden sunset sky + glowing sun + turquoise rolling ocean waves)
func renderOceanBeach(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW, uvH := w/2, h/2

	t := float64(frameIdx) * 0.16
	horizon := 0.46
	sunX := 0.5 + 0.15*math.Sin(float64(seed&15))
	sunY := 0.34

	ySpan := yMax - yMin
	if ySpan > 0 {
		for y := 0; y < h; y++ {
			fy := float64(y) / float64(h)
			row := Y[y*w : (y+1)*w]
			if fy < horizon {
				// Sunset sky + glowing sun
				skyBase := 0.30 + 0.45*(fy/horizon)
				dy := (fy - sunY) * 1.0
				for x := 0; x < w; x++ {
					fx := float64(x) / float64(w)
					dx := (fx - sunX) * (float64(w) / float64(h))
					sunGlow := math.Exp(-(dx*dx+dy*dy)*35.0) * 0.65
					v := skyBase + sunGlow
					if v > 1.0 {
						v = 1.0
					}
					row[x] = clampYUV(int32(yMin+v*ySpan), 16, 235)
				}
			} else {
				// Rolling ocean waves + shimmering sun reflection
				depth := (fy - horizon) / (1.0 - horizon)
				waveFreq := 14.0 / (depth + 0.18)
				for x := 0; x < w; x++ {
					fx := float64(x) / float64(w)
					wVal := 0.5 + 0.5*fastSin((fx*4.0+math.Sin(depth*6.0+t)*0.3+waveFreq*0.05-t*0.35))
					shimmer := math.Exp(-math.Pow((fx-sunX)*6.5, 2.0)) * 0.40 * wVal
					v := 0.22 + 0.35*wVal*(1.0-depth*0.35) + shimmer
					if v > 1.0 {
						v = 1.0
					}
					row[x] = clampYUV(int32(yMin+v*ySpan), 16, 235)
				}
			}
		}
	}

	if drawChroma {
		for cy := 0; cy < uvH; cy++ {
			fy := float64(cy) / float64(uvH)
			uRow := U[cy*uvW : (cy+1)*uvW]
			vRow := V[cy*uvW : (cy+1)*uvW]
			if fy < horizon {
				// Warm coral/orange/gold sunset sky
				ratio := fy / horizon
				uBase := int32(142 - 58*ratio)
				vBase := int32(145 + 48*ratio)
				for cx := 0; cx < uvW; cx++ {
					uRow[cx] = clampYUV(uBase, 24, 232)
					vRow[cx] = clampYUV(vBase, 24, 232)
				}
			} else {
				// Deep turquoise/azure ocean + golden reflection
				depth := (fy - horizon) / (1.0 - horizon)
				for cx := 0; cx < uvW; cx++ {
					fx := float64(cx) / float64(uvW)
					refl := math.Exp(-math.Pow((fx-sunX)*5.5, 2.0))
					uVal := int32(168 - 20*depth - 52*refl)
					vVal := int32(82 + 12*depth + 68*refl)
					uRow[cx] = clampYUV(uVal, 24, 232)
					vRow[cx] = clampYUV(vVal, 24, 232)
				}
			}
		}
	} else {
		fillNeutralUV(U, V)
	}
}

// Theme 6: Flora & Nature (organic phyllotaxis/botanical ripples in emerald, moss & sunlight)
func renderFloraOrganic(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW, uvH := w/2, h/2

	t := float64(frameIdx) * 0.14
	petals := float64(5 + int(seed%5)) // 5..9 botanical petals
	cx0 := float64(w) * 0.5
	cy0 := float64(h) * 0.5
	invR := 1.0 / (float64(h) * 0.55)

	yMid := (yMin + yMax) * 0.5
	yHalf := (yMax - yMin) * 0.5

	if yHalf > 0 {
		for y := 0; y < h; y += 2 {
			dy := (float64(y) - cy0) * invR
			dy2 := dy * dy
			row0 := Y[y*w : (y+1)*w]
			row1 := Y[(y+1)*w : (y+2)*w]
			for x := 0; x < w; x += 2 {
				dx := (float64(x) - cx0) * invR
				r := math.Sqrt(dx*dx + dy2 + 0.001)
				ang := math.Atan2(dy, dx)
				// Organic botanical leaf/petal wave + sunlight veins
				leaf := fastSin((petals*ang+math.Sin(r*4.0-t)*1.8)/(2*math.Pi)) * 0.55
				rings := fastSin((r*6.0-t*0.7+leaf)/(2*math.Pi)) * 0.45
				b := clampYUV(int32(yMid+yHalf*(leaf+rings)), 16, 235)
				row0[x], row0[x+1] = b, b
				row1[x], row1[x+1] = b, b
			}
		}
	}

	if drawChroma {
		for cy := 0; cy < uvH; cy++ {
			dy := (float64(cy*2) - cy0) * invR
			dy2 := dy * dy
			uRow := U[cy*uvW : (cy+1)*uvW]
			vRow := V[cy*uvW : (cy+1)*uvW]
			for cx := 0; cx < uvW; cx++ {
				dx := (float64(cx*2) - cx0) * invR
				r := math.Sqrt(dx*dx + dy2 + 0.001)
				ang := math.Atan2(dy, dx)
				wLeaf := fastSin((petals*ang+r*2.5-t*0.4)/(2*math.Pi))
				// Lush forest green (low U, low V) + warm golden sunlight accents
				uRow[cx] = clampYUV(int32(94.0-26.0*wLeaf), 24, 232)
				vRow[cx] = clampYUV(int32(98.0+24.0*wLeaf+16.0*math.Exp(-r*1.5)), 24, 232)
			}
		}
	} else {
		fillNeutralUV(U, V)
	}
}

func fractalHash2D(ix, iy int, seed uint32) float64 {
	h := uint32(ix)*374761393 ^ uint32(iy)*668265263 ^ seed*2246822519
	h = (h ^ (h >> 13)) * 1274126177
	h ^= h >> 16
	return float64(h&0xFFFF) / 65535.0
}

func fractalNoise2D(x, y float64, seed uint32) float64 {
	ix := int(math.Floor(x))
	iy := int(math.Floor(y))
	fx := x - float64(ix)
	fy := y - float64(iy)
	ux := fx * fx * (3.0 - 2.0*fx)
	uy := fy * fy * (3.0 - 2.0*fy)
	a := fractalHash2D(ix, iy, seed)
	b := fractalHash2D(ix+1, iy, seed)
	c := fractalHash2D(ix, iy+1, seed)
	d := fractalHash2D(ix+1, iy+1, seed)
	return (a*(1-ux)+b*ux)*(1-uy) + (c*(1-ux)+d*ux)*uy
}

func fractalFBM3(x, y float64, seed uint32) float64 {
	v := 0.571 * fractalNoise2D(x, y, seed)
	v += 0.286 * fractalNoise2D(x*2.03+11.3, y*2.03+7.1, seed+17)
	v += 0.143 * fractalNoise2D(x*4.07+23.9, y*4.07+19.4, seed+53)
	return v
}

// Theme 7: Sonho Fractal (Domain-Warped fBm — dreamy watercolor clouds & liquid gradients)
func renderFractalDream(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW, uvH := w/2, h/2
	s32 := uint32(seed ^ (seed >> 32))
	t := float64(frameIdx) * 0.045
	scale := 3.0 + float64((seed>>8)&3)*0.35

	// Coarse 1/8-res Y grid (bilinear upsampled for ultra-fast smooth liquid gradients)
	if yMax > yMin {
		gw := (w + 7) / 8
		gh := (h + 7) / 8
		gridY := make([]float64, (gw+1)*(gh+1))
		ySpan := yMax - yMin
		for gy := 0; gy <= gh; gy++ {
			ny := (float64(gy) / float64(gh)) * scale
			for gx := 0; gx <= gw; gx++ {
				nx := (float64(gx) / float64(gw)) * scale * 1.777
				q := fractalFBM3(nx+t*0.5, ny+t*0.35, s32+77)
				r := fractalFBM3(nx+2.4*q-t*0.3, ny+2.4*q+4.1, s32+409)
				v := math.Max(0, math.Min(1, (r-0.15)*1.5))
				gridY[gy*(gw+1)+gx] = yMin + v*ySpan
			}
		}
		loY, hiY := int32(math.Round(yMin)), int32(math.Round(yMax))
		for y := 0; y < h; y++ {
			fy := float64(y) / 8.0
			iy := int(fy)
			wy := fy - float64(iy)
			r0 := iy * (gw + 1)
			r1 := (iy + 1) * (gw + 1)
			dRow := y * w
			for x := 0; x < w; x++ {
				fx := float64(x) / 8.0
				ix := int(fx)
				wx := fx - float64(ix)
				vy := (gridY[r0+ix]*(1-wx)+gridY[r0+ix+1]*wx)*(1-wy) +
					(gridY[r1+ix]*(1-wx)+gridY[r1+ix+1]*wx)*wy
				Y[dRow+x] = clampYUV(int32(math.Round(vy)), loY, hiY)
			}
		}
	}

	if !drawChroma {
		fillNeutralUV(U, V)
		return
	}

	gw := (uvW + 3) / 4
	gh := (uvH + 3) / 4
	gridCb := make([]float64, (gw+1)*(gh+1))
	gridCr := make([]float64, (gw+1)*(gh+1))
	for gy := 0; gy <= gh; gy++ {
		ny := (float64(gy) / float64(gh)) * scale
		for gx := 0; gx <= gw; gx++ {
			nx := (float64(gx) / float64(gw)) * scale * 1.777
			qx := fractalFBM3(nx+t*0.6, ny-t*0.4, s32)
			qy := fractalFBM3(nx+5.2-t*0.5, ny+1.3+t*0.7, s32+101)
			rx := fractalFBM3(nx+2.2*qx+1.7, ny+2.2*qy+9.2, s32+211)
			ry := fractalFBM3(nx+2.2*qx+8.3, ny+2.2*qy+2.8, s32+307)
			c1 := math.Max(0, math.Min(1, (rx-0.18)*1.55))
			c2 := math.Max(0, math.Min(1, (ry-0.18)*1.55))
			gridCb[gy*(gw+1)+gx] = 48.0 + c1*160.0
			gridCr[gy*(gw+1)+gx] = 48.0 + c2*160.0
		}
	}
	for y := 0; y < uvH; y++ {
		fy := float64(y) / 4.0
		iy := int(fy)
		wy := fy - float64(iy)
		r0 := iy * (gw + 1)
		r1 := (iy + 1) * (gw + 1)
		dRow := y * uvW
		for x := 0; x < uvW; x++ {
			fx := float64(x) / 4.0
			ix := int(fx)
			wx := fx - float64(ix)
			vCb := (gridCb[r0+ix]*(1-wx)+gridCb[r0+ix+1]*wx)*(1-wy) +
				(gridCb[r1+ix]*(1-wx)+gridCb[r1+ix+1]*wx)*wy
			vCr := (gridCr[r0+ix]*(1-wx)+gridCr[r0+ix+1]*wx)*(1-wy) +
				(gridCr[r1+ix]*(1-wx)+gridCr[r1+ix+1]*wx)*wy
			U[dRow+x] = clampYUV(int32(math.Round(vCb)), 44, 212)
			V[dRow+x] = clampYUV(int32(math.Round(vCr)), 44, 212)
		}
	}
}

// RenderFractalChromaFields generates deterministic Domain-Warped fBm chroma planes (Cb, Cr)
// clamped strictly to [lo, hi] so ±delta modulation never clips and blind decoding matches bit-exact.
func RenderFractalChromaFields(uvW, uvH, frameIdx int, seed uint64, lo, hi int) ([]byte, []byte) {
	s32 := uint32(seed ^ (seed >> 32))
	t := float64(frameIdx) * 0.045
	scale := 3.0 + float64((seed>>8)&3)*0.35
	span := float64(hi - lo)

	gw := (uvW + 3) / 4
	gh := (uvH + 3) / 4
	gridCb := make([]float64, (gw+1)*(gh+1))
	gridCr := make([]float64, (gw+1)*(gh+1))
	for gy := 0; gy <= gh; gy++ {
		ny := (float64(gy) / float64(gh)) * scale
		for gx := 0; gx <= gw; gx++ {
			nx := (float64(gx) / float64(gw)) * scale * 1.777
			qx := fractalFBM3(nx+t*0.6, ny-t*0.4, s32)
			qy := fractalFBM3(nx+5.2-t*0.5, ny+1.3+t*0.7, s32+101)
			rx := fractalFBM3(nx+2.2*qx+1.7, ny+2.2*qy+9.2, s32+211)
			ry := fractalFBM3(nx+2.2*qx+8.3, ny+2.2*qy+2.8, s32+307)
			c1 := math.Max(0, math.Min(1, (rx-0.18)*1.55))
			c2 := math.Max(0, math.Min(1, (ry-0.18)*1.55))
			gridCb[gy*(gw+1)+gx] = float64(lo) + c1*span
			gridCr[gy*(gw+1)+gx] = float64(lo) + c2*span
		}
	}

	U := make([]byte, uvW*uvH)
	V := make([]byte, uvW*uvH)
	lo32, hi32 := int32(lo), int32(hi)
	for y := 0; y < uvH; y++ {
		fy := float64(y) / 4.0
		iy := int(fy)
		wy := fy - float64(iy)
		r0 := iy * (gw + 1)
		r1 := (iy + 1) * (gw + 1)
		dRow := y * uvW
		for x := 0; x < uvW; x++ {
			fx := float64(x) / 4.0
			ix := int(fx)
			wx := fx - float64(ix)
			vCb := (gridCb[r0+ix]*(1-wx)+gridCb[r0+ix+1]*wx)*(1-wy) +
				(gridCb[r1+ix]*(1-wx)+gridCb[r1+ix+1]*wx)*wy
			vCr := (gridCr[r0+ix]*(1-wx)+gridCr[r0+ix+1]*wx)*(1-wy) +
				(gridCr[r1+ix]*(1-wx)+gridCr[r1+ix+1]*wx)*wy
			U[dRow+x] = clampYUV(int32(math.Round(vCb)), lo32, hi32)
			V[dRow+x] = clampYUV(int32(math.Round(vCr)), lo32, hi32)
		}
	}
	return U, V
}

// RenderThemeChromaFields generates deterministic chroma planes (Cb, Cr) for ANY procedural art theme
// (Fractal, Waves, Lightning, Vortex, Beach, Flora, Aurora) mapped cleanly into [lo, hi] so ±delta
// modulation over a host video's luminance stays blind-decodable and clip-free.
func RenderThemeChromaFields(uvW, uvH, frameIdx int, seed uint64, theme uint8, lo, hi int) ([]byte, []byte) {
	if theme == ArtThemeNone {
		U := make([]byte, uvW*uvH)
		V := make([]byte, uvW*uvH)
		fillNeutralUV(U, V)
		return U, V
	}
	if theme == ArtThemeAuto || theme == ArtThemeFractal || theme > ArtThemeJuliaTuring {
		return RenderFractalChromaFields(uvW, uvH, frameIdx, seed, lo, hi)
	}
	tmp := NewYUVFrame(uvW*2, uvH*2)
	// yMin == yMax skips luma work and renders only the deterministic chroma planes (U, V) in [24..232].
	RenderProceduralArt(tmp, frameIdx, seed, theme, 128.0, 128.0, true)
	rawU := tmp.UPlane()
	rawV := tmp.VPlane()
	U := make([]byte, uvW*uvH)
	V := make([]byte, uvW*uvH)
	span := hi - lo
	lo32, hi32 := int32(lo), int32(hi)
	for i := range U {
		uScaled := int32(lo) + (int32(rawU[i])-24)*int32(span)/208
		vScaled := int32(lo) + (int32(rawV[i])-24)*int32(span)/208
		U[i] = clampYUV(uScaled, lo32, hi32)
		V[i] = clampYUV(vScaled, lo32, hi32)
	}
	return U, V
}

func fillNeutralUV(U, V []byte) {
	for i := range U {
		U[i] = 128
		V[i] = 128
	}
}

// SynthAmbientMusicWAV synthesizes a warm, evolving stereo pentatonic/Lydian ambient soundtrack
// in pure Go with zero reflection and a 4096-entry sine LUT (<15ms for a 16s seamless loop!).
func SynthAmbientMusicWAV(durationSec float64, seed uint64) []byte {
	const sr = 44100
	if durationSec < 1.5 {
		durationSec = 1.5
	}
	seamlessLoop := false
	if durationSec > 16.0 {
		durationSec = 16.0
		seamlessLoop = true
	}

	nSamples := int(math.Ceil(durationSec * float64(sr)))
	dataBytes := nSamples * 4
	out := make([]byte, 44+dataBytes)

	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+dataBytes))
	copy(out[8:16], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1)
	binary.LittleEndian.PutUint16(out[22:24], 2)
	binary.LittleEndian.PutUint32(out[24:28], sr)
	binary.LittleEndian.PutUint32(out[28:32], sr*4)
	binary.LittleEndian.PutUint16(out[32:34], 4)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataBytes))

	roots := []float64{130.81, 146.83, 164.81, 174.61, 196.00, 220.00}
	root := roots[seed%uint64(len(roots))]
	chordProg := [4][3]float64{
		{1.0, 1.25, 1.5},
		{0.8409, 1.0, 1.25},
		{0.6674, 1.0, 1.3348},
		{0.7492, 1.1225, 1.5},
	}
	bellRatios := []float64{2.0, 2.2449, 2.5198, 3.0, 3.3636, 4.0}
	chordDur := 4.0
	bellStep := 0.5

	fadeSamples := int(0.12 * float64(sr))
	if seamlessLoop {
		fadeSamples = int(0.04 * float64(sr))
	}
	if fadeSamples > nSamples/3 {
		fadeSamples = nSamples / 3
	}

	var phaseL [3]float64
	var phaseR [3]float64
	var phaseBass float64
	var phaseBell float64
	invSR := 1.0 / float64(sr)

	dst := out[44:]
	for i := 0; i < nSamples; i++ {
		t := float64(i) * invSR
		bar := int(t/chordDur) & 3
		chord := chordProg[(bar+int(seed&3))&3]

		lfo := 0.72 + 0.28*fastSin(0.1875*t)
		var padL, padR float64
		for v := 0; v < 3; v++ {
			f := root * chord[v]
			phaseL[v] += f * (1.0 + 0.0025*float64(v+1)) * invSR
			phaseR[v] += f * (1.0 - 0.0025*float64(v+1)) * invSR
			padL += fastSin(phaseL[v]) + 0.25*fastSin(2.0*phaseL[v])
			padR += fastSin(phaseR[v]) + 0.25*fastSin(2.0*phaseR[v])
		}

		phaseBass += (root * 0.5 * chord[0]) * invSR
		bass := fastSin(phaseBass)

		stepIdx := int(t / bellStep)
		stepLocalT := t - float64(stepIdx)*bellStep
		noteIdx := int(((seed >> uint((stepIdx%6)*4)) + uint64(stepIdx*7)) % uint64(len(bellRatios)))
		fBell := root * bellRatios[noteIdx]
		phaseBell += fBell * invSR
		decay := 1.0 / (1.0 + stepLocalT*5.0 + stepLocalT*stepLocalT*10.0)
		attack := stepLocalT * 40.0
		if attack > 1.0 {
			attack = 1.0
		}
		bell := fastSin(phaseBell) * decay * attack
		pan := 0.5 + 0.35*fastSin(float64(stepIdx)*0.21)

		env := 1.0
		if i < fadeSamples {
			env = float64(i) / float64(fadeSamples)
		} else if i > nSamples-fadeSamples {
			env = float64(nSamples-i) / float64(fadeSamples)
		}

		outL := env * (2600.0*lfo*padL + 2800.0*bass + 2400.0*(1.0-pan)*bell)
		outR := env * (2600.0*lfo*padR + 2800.0*bass + 2400.0*pan*bell)

		var sL, sR int16
		if outL > 32000 {
			sL = 32000
		} else if outL < -32000 {
			sL = -32000
		} else {
			sL = int16(outL)
		}
		if outR > 32000 {
			sR = 32000
		} else if outR < -32000 {
			sR = -32000
		} else {
			sR = int16(outR)
		}
		off := i * 4
		binary.LittleEndian.PutUint16(dst[off:off+2], uint16(sL))
		binary.LittleEndian.PutUint16(dst[off+2:off+4], uint16(sR))
	}
	return out
}

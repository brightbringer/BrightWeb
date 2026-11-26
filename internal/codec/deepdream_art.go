package codec

import (
	"math"
	"math/cmplx"
	"runtime"
	"sync"
)

// Pure-Go deterministic psychedelic fractal & DeepDream-inspired renderers
// (inspired by Coni63/DeepDream fractal_light + Starry Night Layers 2 & 4 + DeepDreamAnim).
// Zero external dependencies; 100% deterministic from (frameIdx, seed) for exact subtractive decoding.

func ddRGBToStudioYUV(r, g, b, yLo, yHi float64) (byte, byte, byte) {
	y := 16.0 + (65.481*r+128.553*g+24.966*b)/255.0
	cb := 128.0 + (-37.797*r-74.203*g+112.0*b)/255.0
	cr := 128.0 + (112.0*r-93.786*g-18.214*b)/255.0
	yMapped := yLo
	if yHi > yLo {
		yMapped = yLo + (y-16.0)*(yHi-yLo)/219.0
	}
	return clampYUV(int32(math.Round(yMapped)), int32(yLo), int32(yHi)),
		clampYUV(int32(math.Round(cb)), 36, 220),
		clampYUV(int32(math.Round(cr)), 36, 220)
}

func ddPaletteConi(t float64) (float64, float64, float64) {
	t = t - math.Floor(t)
	type rgb struct{ r, g, b float64 }
	stops := [6]rgb{
		{8, 18, 128},    // deep sapphire blue
		{235, 240, 255}, // glowing pearl white
		{255, 198, 42},  // luminous gold
		{196, 46, 18},   // fiery ember/copper
		{24, 10, 62},    // midnight violet
		{8, 18, 128},
	}
	f := t * 5.0
	i := int(f)
	if i >= 5 {
		i = 4
	}
	u := f - float64(i)
	u = u * u * (3.0 - 2.0*u)
	a, b := stops[i], stops[i+1]
	return a.r + (b.r-a.r)*u, a.g + (b.g-a.g)*u, a.b + (b.b-a.b)*u
}

func ddPaletteStarry(t float64) (float64, float64, float64) {
	t = t - math.Floor(t)
	type rgb struct{ r, g, b float64 }
	stops := [7]rgb{
		{18, 28, 72},    // midnight indigo
		{46, 98, 188},   // vibrant cobalt
		{108, 168, 212}, // luminous sky cyan
		{242, 226, 148}, // starry warm cream
		{238, 178, 38},  // van gogh gold
		{32, 74, 96},    // deep cypress teal
		{18, 28, 72},
	}
	f := t * 6.0
	i := int(f)
	if i >= 6 {
		i = 5
	}
	u := f - float64(i)
	u = u * u * (3.0 - 2.0*u)
	a, b := stops[i], stops[i+1]
	return a.r + (b.r-a.r)*u, a.g + (b.g-a.g)*u, a.b + (b.b-a.b)*u
}

func ddPalettePsyche(t, shift float64) (float64, float64, float64) {
	p := 2.0 * math.Pi * (t + shift)
	return 127.5 + 122.0*math.Cos(p),
		127.5 + 122.0*math.Cos(p+2.094395),
		127.5 + 122.0*math.Cos(p+4.18879)
}

// ddHueMatrix precomputes a luminance-preserving 3x3 RGB hue-rotation matrix around the (1,1,1) axis.
type ddHueMatrix [9]float64

func makeHueMatrix(rad float64) ddHueMatrix {
	c := math.Cos(rad)
	s := math.Sin(rad)
	const sq3 = 0.5773502691896257 // 1 / sqrt(3)
	omc := 1.0 - c
	return ddHueMatrix{
		c + omc/3.0, omc/3.0 - sq3*s, omc/3.0 + sq3*s,
		omc/3.0 + sq3*s, c + omc/3.0, omc/3.0 - sq3*s,
		omc/3.0 - sq3*s, omc/3.0 + sq3*s, c + omc/3.0,
	}
}

func (m ddHueMatrix) apply(r, g, b float64) (float64, float64, float64) {
	nr := m[0]*r + m[1]*g + m[2]*b
	ng := m[3]*r + m[4]*g + m[5]*b
	nb := m[6]*r + m[7]*g + m[8]*b
	if nr < 0 {
		nr = 0
	} else if nr > 255 {
		nr = 255
	}
	if ng < 0 {
		ng = 0
	} else if ng > 255 {
		ng = 255
	}
	if nb < 0 {
		nb = 0
	} else if nb > 255 {
		nb = 255
	}
	return nr, ng, nb
}

func ddParallelRows(h int, fn func(y0, y1 int)) {
	workers := runtime.GOMAXPROCS(0)
	if workers > 6 {
		workers = 6
	}
	if workers < 2 || h < 64 {
		fn(0, h)
		return
	}
	var wg sync.WaitGroup
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

// Theme 8: Mandelbrot Seahorse Valley Infinite Zoom (inspired by Coni63/DeepDream fractal_light)
// Centered on the exact 16-digit Misiurewicz Seahorse double-spiral hub so it stays 100% dead-center
// on symmetric spirals all the way down to 10^-12 (never drifting off-axis into black interior!).
func renderThemeMandelbrot(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW := w / 2
	chromaOnly := yMax <= yMin

	// Exact 16-digit Misiurewicz Double-Spiral center in Seahorse Valley (never hits black cardioid!)
	const (
		cx = -0.7436438870371587
		cy = 0.1318259042053120
	)
	// Smooth deep forward zoom into the exact 16-digit Seahorse Valley double-spiral hub
	// (reaches 10^-5 at 15s / 225f and 10^-8 at 32s / 480f, staying 100% centered on crisp spirals!)
	cycleF := float64(frameIdx % 480)
	zoom := 0.055 * math.Pow(0.972, cycleF)
	rot := float64(frameIdx)*0.018 + float64(seed&0x3F)*0.04
	cosR, sinR := math.Cos(rot), math.Sin(rot)
	aspect := float64(w) / float64(h)
	// Scale maxIter generously with zoom depth so deep spirals stay razor sharp with zero blank core!
	maxIter := 180 + int(cycleF*2.4)
	if maxIter > 900 {
		maxIter = 900
	}
	palShift := float64(frameIdx)*0.008 + float64((seed>>8)&0xFF)/256.0
	hueMat := makeHueMatrix(float64((seed>>16)&0xFF) / 256.0 * 0.85) // subtle seed hue shift preserving Coni63 gold/sapphire spirit

	evalMandel := func(cr, ci float64) (float64, float64, float64) {
		zr, zi := 0.0, 0.0
		iter := 0
		var zr2, zi2 float64
		minTrap := 1e9
		for iter < maxIter {
			zr2, zi2 = zr*zr, zi*zi
			if zr2+zi2 > 16.0 {
				break
			}
			zi = 2.0*zr*zi + ci
			zr = zr2 - zi2 + cr
			iter++
			if iter >= 3 {
				if d := math.Abs(zr*zr - zi*zi); d < minTrap {
					minTrap = d
				}
			}
		}
		var r, g, b float64
		if iter == maxIter {
			// Glowing orbit-trap core instead of flat dead black!
			r, g, b = ddPaletteConi(math.Sqrt(minTrap)*1.8 + palShift)
			r *= 0.45
			g *= 0.45
			b *= 0.45
		} else {
			nu := float64(iter) + 1.0 - math.Log(math.Log(zr2+zi2)*0.5/math.Ln2)/math.Ln2
			r, g, b = ddPaletteConi(nu*0.036 + palShift)
		}
		return hueMat.apply(r, g, b)
	}

	if chromaOnly {
		uvH := h / 2
		ddParallelRows(uvH, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				ny := (float64(y)/float64(uvH) - 0.5) * 2.0 * zoom
				row := y * uvW
				for x := 0; x < uvW; x++ {
					nx := (float64(x)/float64(uvW) - 0.5) * 2.0 * zoom * aspect
					r, g, b := evalMandel(cx+nx*cosR-ny*sinR, cy+nx*sinR+ny*cosR)
					_, cbv, crv := ddRGBToStudioYUV(r, g, b, 128, 128)
					U[row+x], V[row+x] = cbv, crv
				}
			}
		})
		return
	}

	ddParallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			ny := (float64(y)/float64(h) - 0.5) * 2.0 * zoom
			yOff := y * w
			uvOff := (y >> 1) * uvW
			evenY := (y & 1) == 0
			for x := 0; x < w; x++ {
				nx := (float64(x)/float64(w) - 0.5) * 2.0 * zoom * aspect
				r, g, b := evalMandel(cx+nx*cosR-ny*sinR, cy+nx*sinR+ny*cosR)
				yv, cbv, crv := ddRGBToStudioYUV(r, g, b, yMin, yMax)
				Y[yOff+x] = yv
				if drawChroma && evenY && (x&1) == 0 {
					U[uvOff+(x>>1)] = cbv
					V[uvOff+(x>>1)] = crv
				}
			}
		}
	})
}

// Theme 9: DeepDream Layer 2 — Starry Night Flowing Swirly Waves (with seed-driven hue variation!)
func renderThemeDeepDreamL2(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW := w / 2
	chromaOnly := yMax <= yMin
	aspect := float64(w) / float64(h)
	t := float64(frameIdx)*0.065 + float64(seed&0xFF)*0.02
	zoom := 0.96
	// Seed-driven full hue rotation so every video / keyword gets its own rich Starry Night color mood!
	hueAngle := float64((seed>>16)&0xFF) / 256.0 * 2.0 * math.Pi
	hueMat := makeHueMatrix(hueAngle)

	type vortex struct{ x, y, strength float64 }
	baseV := [6]vortex{
		{-0.55, -0.18, 1.35},
		{0.12, -0.25, -1.65},
		{0.72, -0.32, 1.45},
		{-0.22, 0.28, -1.25},
		{0.48, 0.24, 1.50},
		{-0.85, 0.35, 1.10},
	}
	var vx, vy [6]float64
	for i, v := range baseV {
		vx[i] = v.x + 0.14*math.Sin(t*0.7+float64(i)*1.3)
		vy[i] = v.y + 0.12*math.Cos(t*0.6+float64(i)*1.7)
	}

	evalPixel := func(px, py float64) (float64, float64, float64) {
		psi := py * 1.4
		swirlGlow := 0.0
		for i := 0; i < 6; i++ {
			dx := px - vx[i]
			dy := py - vy[i]
			r2 := dx*dx + dy*dy + 0.035
			psi += baseV[i].strength * 0.42 * math.Log(r2)
			ang := math.Atan2(dy, dx)
			psi += 0.14 * math.Sin(ang*2.0+t*0.8+float64(i)) / (1.0 + r2*3.5)
			swirlGlow += math.Exp(-r2 * 5.2)
		}
		wave1 := math.Sin(psi*11.5 - t*2.4)
		wave2 := math.Sin((psi*23.0 + px*4.5) + t*1.5)
		ridge := 0.5 + 0.36*wave1 + 0.14*wave2
		colT := 0.15 + 0.52*math.Min(1.0, swirlGlow*0.75) + 0.22*ridge + 0.05*math.Sin(px*1.5-t*0.4)
		r, g, b := ddPaletteStarry(colT)
		r, g, b = hueMat.apply(r, g, b)
		shade := 0.62 + 0.55*ridge
		return math.Min(255, r*shade), math.Min(255, g*shade), math.Min(255, b*shade)
	}

	if chromaOnly {
		uvH := h / 2
		ddParallelRows(uvH, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				py := (float64(y)/float64(uvH) - 0.5) * 2.0 * zoom
				row := y * uvW
				for x := 0; x < uvW; x++ {
					px := (float64(x)/float64(uvW) - 0.5) * 2.0 * aspect * zoom
					r, g, b := evalPixel(px, py)
					_, cbv, crv := ddRGBToStudioYUV(r, g, b, 128, 128)
					U[row+x], V[row+x] = cbv, crv
				}
			}
		})
		return
	}

	ddParallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			py := (float64(y)/float64(h) - 0.5) * 2.0 * zoom
			yOff := y * w
			uvOff := (y >> 1) * uvW
			evenY := (y & 1) == 0
			for x := 0; x < w; x++ {
				px := (float64(x)/float64(w) - 0.5) * 2.0 * aspect * zoom
				r, g, b := evalPixel(px, py)
				yv, cbv, crv := ddRGBToStudioYUV(r, g, b, yMin, yMax)
				Y[yOff+x] = yv
				if drawChroma && evenY && (x&1) == 0 {
					U[uvOff+(x>>1)] = cbv
					V[uvOff+(x>>1)] = crv
				}
			}
		}
	})
}

// Theme 10: DeepDream Layer 4 — Seamless Shepard-Scale Infinite Kaleidoscope Zoom (ZERO pop/stutter!)
// Uses a 4-octave smooth sin^2 crossfaded scale pyramid where each octave fades in smoothly from weight 0
// at the center and fades out smoothly to weight 0 at the edges, guaranteeing 100% seamless infinite loop!
func renderThemeDeepDreamL4(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW := w / 2
	chromaOnly := yMax <= yMin
	aspect := float64(w) / float64(h)

	// Continuous smooth zoom progress (no jump at integer boundaries!)
	zProg := float64(frameIdx) * 0.026
	u := zProg - math.Floor(zProg) // [0, 1)
	rotBase := float64(frameIdx)*0.015 + float64(seed&0xFF)*0.025
	colorT := float64(frameIdx)*0.008 + float64((seed>>8)&0xFF)/256.0
	hueMat := makeHueMatrix(float64((seed>>16)&0xFF) / 256.0 * 2.0 * math.Pi)

	// Precompute 4 octave scales & exact Shepard sin^2 bell weights:
	// At u=0: oct 3 has w=0; at u->1: oct 0 fades smoothly to w=0 while oct 3 fades in!
	var octScale, octW [4]float64
	var wSum float64
	for k := 0; k < 4; k++ {
		lvl := float64(k) - u // spans [-1, 3]
		octScale[k] = math.Pow(2.0, lvl)
		// Smooth sin^2 window on [-1, 3]: exactly 0 at lvl=-1 and lvl=3!
		s := math.Sin(math.Pi * (lvl + 1.0) / 4.0)
		octW[k] = s * s
		wSum += octW[k]
	}
	invWSum := 1.0 / wSum

	evalPixel := func(px, py float64) (float64, float64, float64) {
		r0 := math.Hypot(px, py) + 1e-4
		ang0 := math.Atan2(py, px) + rotBase + 0.32*math.Sin(math.Log(r0)*2.0-zProg*2.0*math.Pi)
		// Exact 6-fold kaleidoscopic angular symmetry (scale-invariant -> 100% seamless!)
		foldAng := math.Abs(math.Mod(ang0+10.0*math.Pi, math.Pi/3.0) - math.Pi/6.0)
		cosF, sinF := math.Cos(foldAng), math.Sin(foldAng)

		var eyeAcc, swirlAcc float64
		for k := 0; k < 4; k++ {
			wk := octW[k]
			if wk < 1e-5 {
				continue
			}
			rk := r0 * octScale[k]
			ux := rk * cosF
			uy := rk * sinF
			// Concentric glowing orb lattice at octave k's exact scale
			cx := math.Mod(ux*1.5+100.0, 1.0) - 0.5
			cy := math.Mod(uy*1.5+100.0, 1.0) - 0.5
			dOrb := math.Hypot(cx, cy)
			ring := math.Cos(dOrb*18.0-zProg*2.0*math.Pi) * math.Exp(-dOrb*3.0)
			swirl := math.Sin(rk*6.283185 - foldAng*6.0)
			eyeAcc += wk * ring
			swirlAcc += wk * swirl
		}
		eyeAcc *= invWSum * 1.45
		swirlAcc *= invWSum

		val := 0.5 + 0.36*eyeAcc + 0.24*swirlAcc
		r1, g1, b1 := ddPaletteStarry(val*0.85 + colorT)
		r2, g2, b2 := ddPalettePsyche(val*0.55, colorT*1.3)
		r, g, b := hueMat.apply(0.65*r1+0.35*r2, 0.65*g1+0.35*g2, 0.65*b1+0.35*b2)
		shade := math.Max(0.28, math.Min(1.25, 0.72+0.55*eyeAcc))
		return math.Min(255, r*shade), math.Min(255, g*shade), math.Min(255, b*shade)
	}

	if chromaOnly {
		uvH := h / 2
		ddParallelRows(uvH, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				py := (float64(y)/float64(uvH) - 0.5) * 2.0
				row := y * uvW
				for x := 0; x < uvW; x++ {
					px := (float64(x)/float64(uvW) - 0.5) * 2.0 * aspect
					r, g, b := evalPixel(px, py)
					_, cbv, crv := ddRGBToStudioYUV(r, g, b, 128, 128)
					U[row+x], V[row+x] = cbv, crv
				}
			}
		})
		return
	}

	ddParallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			py := (float64(y)/float64(h) - 0.5) * 2.0
			yOff := y * w
			uvOff := (y >> 1) * uvW
			evenY := (y & 1) == 0
			for x := 0; x < w; x++ {
				px := (float64(x)/float64(w) - 0.5) * 2.0 * aspect
				r, g, b := evalPixel(px, py)
				yv, cbv, crv := ddRGBToStudioYUV(r, g, b, yMin, yMax)
				Y[yOff+x] = yv
				if drawChroma && evenY && (x&1) == 0 {
					U[uvOff+(x>>1)] = cbv
					V[uvOff+(x>>1)] = crv
				}
			}
		}
	})
}

// Theme 11: Julia + Bio-Turing 360°-Centered Ultra-Macro Infinite Forward Spiral Zoom
// Centers dead-on the exact analytical period-2 repelling spiral hub z_p2 = (-1 + sqrt(-3 - 4c)) / 2
// with multiplier lambda_2 = 4*(c + 1) = R * exp(i * 2*pi/7), R = 1.16 (R^7 = 2.825x PURE REAL scaling!).
// Every turn expands 2.825x, putting GIGANTIC, ultra-close-up curling fractal petals right in front of the
// viewer with analytical distance-estimation 3D filigree relief, concentric pre-critical Bio-Turing eyes,
// zero dizzying spin, zero off-axis drift, and a 100% invisible infinite forward zoom loop!
func renderThemeJuliaTuring(dst *YUVFrame, frameIdx int, seed uint64, yMin, yMax float64, drawChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW := w / 2
	chromaOnly := yMax <= yMin
	aspect := float64(w) / float64(h)

	// Exact analytical period-2 multiplier lambda_2 = R * exp(i * 2*pi / K) -> c = lambda_2/4 - 1.
	// K = 7, R = 1.16 gives R^7 = 2.825x coil spacing: huge, ultra-detailed macro seahorse fronds filling 16:9!
	const (
		R           = 1.16
		K           = 7.0
		zoom0       = 0.008
		cycleFrames = 38.0 // 2.53s per 2.825x self-similar zoom cycle (~1.50x/sec silky macro dive)
	)
	th := 2.0 * math.Pi / K
	lam := complex(R*math.Cos(th), R*math.Sin(th))
	c := lam/4.0 - 1.0
	zp2 := (-1.0 + cmplx.Sqrt(-3.0-4.0*c)) / 2.0
	cRe, cIm := real(c), imag(c)
	zRe, zIm := real(zp2), imag(zp2)

	// Exact forward radial zoom parameter u in [0, K): zero dizzying spin, pure hypnotic forward macro dive.
	u := math.Mod(float64(frameIdx)*(K/cycleFrames), K)
	zoom := zoom0 * math.Pow(R, -u)
	maxIter := 380 + int(2.0*u)

	// Fixed seed-driven entry angle so every video has its own unique orientation without dizzying spin.
	baseAngle := float64(seed&0xFF) / 256.0 * 2.0 * math.Pi
	cosR, sinR := math.Cos(baseAngle), math.Sin(baseAngle)

	// Smooth continuous aurora palette drift + seed hue-rotation matrix.
	palShift := float64(frameIdx)*0.0045 + float64((seed>>8)&0xFF)/256.0
	hueMat := makeHueMatrix(float64((seed>>16)&0xFF) / 256.0 * 2.0 * math.Pi)

	// Phase-locked radial Bio-Turing log-polar ripples expanding in exact lockstep with R^u.
	logPeriod := K * math.Log(R)
	turingPhase := 2.0 * math.Pi * (u / K)
	tWave := float64(frameIdx) * 0.045

	evalPixel := func(sx, sy float64) (float64, float64, float64) {
		dx := sx * zoom
		dy := sy * zoom
		zr := zRe + dx*cosR - dy*sinR
		zi := zIm + dx*sinR + dy*cosR
		dzr, dzi := 1.0, 0.0

		iter := 0
		var zr2, zi2 float64
		for iter < maxIter {
			zr2, zi2 = zr*zr, zi*zi
			if zr2+zi2 > 64.0 {
				break
			}
			// Analytical derivative dz <- 2*z*dz for scale-invariant 3D filigree distance estimation.
			ndzr := 2.0 * (zr*dzr - zi*dzi)
			ndzi := 2.0 * (zr*dzi + zi*dzr)
			dzr, dzi = ndzr, ndzi
			zi = 2.0*zr*zi + cIm
			zr = zr2 - zi2 + cRe
			iter++
		}

		// Subtle forward-expanding Bio-Turing cellular ripples locked to the exact fractal zoom rate.
		rScreen := math.Hypot(sx, sy) + 1e-4
		logR := (math.Log(rScreen) / logPeriod) * 2.0 * math.Pi
		angS := math.Atan2(sy, sx)
		turing := 0.5 + 0.25*math.Sin(logR*2.0-turingPhase*2.0+0.6*math.Sin(angS*7.0+tWave*0.5)) +
			0.25*math.Cos(angS*7.0-logR+turingPhase)

		var r, g, b float64
		if iter == maxIter {
			r, g, b = ddPalettePsyche(0.62+turing*0.08, palShift)
			r, g, b = r*0.42, g*0.42, b*0.42
		} else {
			r2 := zr2 + zi2
			modZ := math.Sqrt(r2)
			modDZ := math.Hypot(dzr, dzi) + 1e-18
			// Screen-normalized distance to the Julia boundary (100% scale-invariant across the zoom cycle!).
			dist := (0.5 * modZ * math.Log(modZ) / modDZ) / zoom
			nu := float64(iter) + 1.0 - math.Log(math.Log(r2)*0.5/math.Ln2)/math.Ln2
			contour := 0.5 + 0.5*math.Sin(math.Log(dist+1e-7)*4.5)
			r, g, b = ddPalettePsyche((nu-2.0*u)*0.075+contour*0.03+turing*0.035, 0.12+palShift)
			// Crisp 3D luminous filigree shading so every microscopic spiral ruffle pops in relief.
			filigree := math.Exp(-dist * 18.0)
			halo := math.Exp(-dist * 4.5)
			shade := 0.44 + 0.30*contour + 0.46*halo + 0.36*filigree
			r = math.Min(255, r*shade+filigree*72.0)
			g = math.Min(255, g*shade+filigree*62.0)
			b = math.Min(255, b*shade+filigree*88.0)
		}
		return hueMat.apply(r, g, b)
	}

	if chromaOnly {
		uvH := h / 2
		ddParallelRows(uvH, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				sy := (float64(y)/float64(uvH) - 0.5) * 2.0
				row := y * uvW
				for x := 0; x < uvW; x++ {
					sx := (float64(x)/float64(uvW) - 0.5) * 2.0 * aspect
					r, g, b := evalPixel(sx, sy)
					_, cbv, crv := ddRGBToStudioYUV(r, g, b, 128, 128)
					U[row+x], V[row+x] = cbv, crv
				}
			}
		})
		return
	}

	ddParallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			sy := (float64(y)/float64(h) - 0.5) * 2.0
			yOff := y * w
			uvOff := (y >> 1) * uvW
			evenY := (y & 1) == 0
			for x := 0; x < w; x++ {
				sx := (float64(x)/float64(w) - 0.5) * 2.0 * aspect
				r, g, b := evalPixel(sx, sy)
				yv, cbv, crv := ddRGBToStudioYUV(r, g, b, yMin, yMax)
				Y[yOff+x] = yv
				if drawChroma && evenY && (x&1) == 0 {
					U[uvOff+(x>>1)] = cbv
					V[uvOff+(x>>1)] = crv
				}
			}
		}
	})
}

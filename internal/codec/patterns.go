package codec

import "math"

// Precomputed 8x8 DCT-aligned geometric & angular pattern tables for YTCloud's
// lightweight artistic profiles (validated 100% against YouTube VP9 & H.264).
var (
	patDCTX      [64]float64
	patDCTY      [64]float64
	patAngBases  [4][64]float64
	patAngAmps   = [4]float64{46.0, 46.0, 56.0, 56.0}
	patAngFlat   [8][64]byte
	patAngFloat  [8][64]float64
	patBauhausB  [8][64]byte
	patBauhausF  [8][64]float64
	patConstelB  [8][64]byte
	patConstelF  [8][64]float64
	patTruchetB  [4][64]byte
	patTruchetF  [4][64]float64
	patFractal3D = [4]int{-48, -16, 16, 48}
)

func init() {
	// 1. Lowest-frequency DCT(1,0) and DCT(0,1) bases for art-crystal (Cristal 3D 4-Angulos + DC)
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			patDCTX[y*8+x] = math.Cos(math.Pi * (float64(x) + 0.5) / 8.0)
			patDCTY[y*8+x] = math.Cos(math.Pi * (float64(y) + 0.5) / 8.0)
		}
	}

	// 2. 4 orthogonal 8x8 line/angle wave bases (0, 90, 45, 135 deg)
	for a := 0; a < 4; a++ {
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				fx, fy := float64(x)+0.5, float64(y)+0.5
				var ph float64
				switch a {
				case 0: // horizontal
					ph = 2.0 * math.Pi * fy / 8.0
				case 1: // vertical
					ph = 2.0 * math.Pi * fx / 8.0
				case 2: // 45 deg diagonal
					ph = 2.0 * math.Pi * (fx - fy) / 8.0
				case 3: // 135 deg anti-diagonal
					ph = 2.0 * math.Pi * (fx + fy) / 8.0
				}
				patAngBases[a][y*8+x] = math.Cos(ph)
			}
		}
	}

	// 3. art-angles (4 angles x 2 DC levels = 3b/cell, ±58 wave — 100% VP9 validated!)
	for sym := 0; sym < 8; sym++ {
		a := sym & 3
		dc := 84.0
		if (sym>>2)&1 == 1 {
			dc = 172.0
		}
		for i := 0; i < 64; i++ {
			v := clampPatternVal(dc + 58.0*patAngBases[a][i])
			patAngFloat[sym][i] = v
			patAngFlat[sym][i] = byte(math.Round(v))
		}
	}

	// 4. art-bauhaus (4 diagonal/orthogonal cuts x 2 polarities = 3b/cell)
	for sym := 0; sym < 8; sym++ {
		cut := sym & 3
		pol := (sym >> 2) & 1
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				cx, cy := float64(x)-3.5, float64(y)-3.5
				var signed float64
				switch cut {
				case 0:
					signed = cx - cy
				case 1:
					signed = cx + cy
				case 2:
					signed = cy
				case 3:
					signed = cx
				}
				if pol == 1 {
					signed = -signed
				}
				alpha := math.Max(0, math.Min(1, 0.5+signed*0.55))
				v := clampPatternVal(56.0 + alpha*144.0)
				patBauhausF[sym][y*8+x] = v
				patBauhausB[sym][y*8+x] = byte(math.Round(v))
			}
		}
	}

	// 5. art-constel (4 quadrant orb positions x 2 polarities = 3b/cell)
	for sym := 0; sym < 8; sym++ {
		quad := sym & 3
		pol := (sym >> 2) & 1
		ox := 2.2
		if quad&1 == 1 {
			ox = 5.8
		}
		oy := 2.2
		if (quad>>1)&1 == 1 {
			oy = 5.8
		}
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				d := math.Hypot(float64(x)+0.5-ox, float64(y)+0.5-oy)
				orb := math.Max(0, math.Min(1, (2.7-d)/1.4))
				var v float64
				if pol == 0 {
					v = 48.0 + orb*156.0
				} else {
					v = 204.0 - orb*156.0
				}
				v = clampPatternVal(v)
				patConstelF[sym][y*8+x] = v
				patConstelB[sym][y*8+x] = byte(math.Round(v))
			}
		}
	}

	// 6. art-truchet (2 seamless quarter-circle Smith Truchet arcs x 2 high-contrast tiers = 2b/cell)
	for sym := 0; sym < 4; sym++ {
		orient := sym & 1
		tier := (sym >> 1) & 1
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				fx, fy := float64(x)+0.5, float64(y)+0.5
				r := 4.0
				var dist float64
				if orient == 0 {
					dist = math.Min(math.Abs(math.Hypot(fx, fy)-r), math.Abs(math.Hypot(fx-8, fy-8)-r))
				} else {
					dist = math.Min(math.Abs(math.Hypot(fx-8, fy)-r), math.Abs(math.Hypot(fx, fy-8)-r))
				}
				rib := math.Max(0, math.Min(1, (1.85-dist)/1.05))
				var v float64
				if tier == 0 {
					v = 24.0 + rib*152.0
				} else {
					v = 104.0 + rib*134.0
				}
				v = clampPatternVal(v)
				patTruchetF[sym][y*8+x] = v
				patTruchetB[sym][y*8+x] = byte(math.Round(v))
			}
		}
	}
}

func clampPatternVal(v float64) float64 {
	if v < 16 {
		return 16
	}
	if v > 240 {
		return 240
	}
	return v
}

// IsGeometricArtMode returns true for the new 8x8 DCT-aligned geometric/angular/3D-fractal modes.
func IsGeometricArtMode(mode string) bool {
	switch mode {
	case "art-crystal", "art-fractal3d", "art-angles", "art-fractal-angles", "art-bauhaus", "art-constel", "art-truchet":
		return true
	default:
		return false
	}
}

// CanHideHeaderArtMode reports whether an ArtMode supports seamless hidden headers (FlagHiddenHeader).
func CanHideHeaderArtMode(mode string, hasHost bool) bool {
	switch mode {
	case "video-fractal-chroma", "video-chroma-q4", "subtractive-y2", "art-fullres":
		return true
	case "video-bw":
		return hasHost
	default:
		return IsGeometricArtMode(mode) || IsStealthArtMode(mode)
	}
}

// writeScaledPatch8 writes an 8x8 patch into a bs x bs cell (bs=8 native 1:1, or bs=16 2x nearest-upscaled for Subtle).
func writeScaledPatch8(Y []byte, stride, px, py, bs int, patch *[64]byte) {
	if bs == 8 {
		for dy := 0; dy < 8; dy++ {
			copy(Y[(py+dy)*stride+px:(py+dy)*stride+px+8], patch[dy*8:dy*8+8])
		}
		return
	}
	for dy := 0; dy < bs; dy++ {
		sy := (dy * 8) / bs
		row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bs]
		pOff := sy * 8
		for dx := 0; dx < bs; dx++ {
			row[dx] = patch[pOff+(dx*8)/bs]
		}
	}
}

// sampleScaledBlock8 averages a bs x bs cell down to an 8x8 float64 grid for matched-filter decoding.
func sampleScaledBlock8(Y []byte, stride, px, py, bs int, dst *[64]float64) {
	if bs == 8 {
		for dy := 0; dy < 8; dy++ {
			row := Y[(py+dy)*stride+px : (py+dy)*stride+px+8]
			dOff := dy * 8
			for dx := 0; dx < 8; dx++ {
				dst[dOff+dx] = float64(row[dx])
			}
		}
		return
	}
	scale := bs / 8
	if scale < 1 {
		scale = 1
	}
	invArea := 1.0 / float64(scale*scale)
	for sy := 0; sy < 8; sy++ {
		for sx := 0; sx < 8; sx++ {
			var sum int
			y0 := py + sy*scale
			x0 := px + sx*scale
			for oy := 0; oy < scale; oy++ {
				row := Y[(y0+oy)*stride+x0 : (y0+oy)*stride+x0+scale]
				for ox := 0; ox < scale; ox++ {
					sum += int(row[ox])
				}
			}
			dst[sy*8+sx] = float64(sum) * invArea
		}
	}
}

func encodeGeometricCell(mode string, Y []byte, stride, px, py, bs, sym int) {
	switch mode {
	case "art-crystal":
		sector := sym & 3
		dc := 84.0
		if (sym>>2)&1 == 1 {
			dc = 172.0
		}
		var cx, cy float64
		switch sector {
		case 0:
			cx, cy = 46.0, 0
		case 1:
			cx, cy = 0, 46.0
		case 2:
			cx, cy = -46.0, 0
		case 3:
			cx, cy = 0, -46.0
		}
		var patch [64]byte
		for i := 0; i < 64; i++ {
			patch[i] = byte(math.Round(clampPatternVal(dc + cx*patDCTX[i] + cy*patDCTY[i])))
		}
		writeScaledPatch8(Y, stride, px, py, bs, &patch)

	case "art-angles":
		writeScaledPatch8(Y, stride, px, py, bs, &patAngFlat[sym&7])

	case "art-bauhaus":
		writeScaledPatch8(Y, stride, px, py, bs, &patBauhausB[sym&7])

	case "art-constel":
		writeScaledPatch8(Y, stride, px, py, bs, &patConstelB[sym&7])

	case "art-truchet":
		writeScaledPatch8(Y, stride, px, py, bs, &patTruchetB[sym&3])

	case "art-fractal-angles":
		// Compute cell average luminance from underlying 3D art or host video and etch zero-mean angle wave!
		var sum int
		for dy := 0; dy < bs; dy++ {
			row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bs]
			for dx := 0; dx < bs; dx++ {
				sum += int(row[dx])
			}
		}
		base := float64(sum) / float64(bs*bs)
		if base < 72.0 {
			base = 72.0
		} else if base > 184.0 {
			base = 184.0
		}
		a := sym & 3
		amp := patAngAmps[a]
		b := &patAngBases[a]
		var patch [64]byte
		for i := 0; i < 64; i++ {
			patch[i] = byte(math.Round(clampPatternVal(base + amp*b[i])))
		}
		writeScaledPatch8(Y, stride, px, py, bs, &patch)
	}
}

func decodeGeometricCell(mode string, Y []byte, stride, px, py, bs int) int {
	var blk [64]float64
	sampleScaledBlock8(Y, stride, px, py, bs, &blk)

	switch mode {
	case "art-crystal":
		var mean, gx, gy float64
		for i := 0; i < 64; i++ {
			v := blk[i]
			mean += v
			gx += v * patDCTX[i]
			gy += v * patDCTY[i]
		}
		dcBit := 0
		if mean/64.0 >= 128.0 {
			dcBit = 4
		}
		ang := math.Atan2(gy, gx)
		if ang < 0 {
			ang += 2.0 * math.Pi
		}
		sector := int(math.Floor((ang+math.Pi/4.0)/(math.Pi/2.0))) & 3
		return dcBit | sector

	case "art-angles":
		return matchTemplate8(&blk, patAngFloat[:])

	case "art-bauhaus":
		return matchTemplate8(&blk, patBauhausF[:])

	case "art-constel":
		return matchTemplate8(&blk, patConstelF[:])

	case "art-truchet":
		return matchTemplate8(&blk, patTruchetF[:])

	case "art-fractal-angles":
		var mean float64
		for i := 0; i < 64; i++ {
			mean += blk[i]
		}
		mean /= 64.0
		bestSym, bestDot := 0, -1e18
		for a := 0; a < 4; a++ {
			b := &patAngBases[a]
			var dot float64
			for i := 0; i < 64; i++ {
				dot += (blk[i] - mean) * b[i]
			}
			if a >= 2 {
				dot *= 1.12 // compensate VP9 diagonal attenuation
			}
			if dot > bestDot {
				bestDot, bestSym = dot, a
			}
		}
		return bestSym
	}
	return 0
}

func matchTemplate8(blk *[64]float64, tmpls [][64]float64) int {
	bestSym := 0
	bestErr := math.MaxFloat64
	for s := range tmpls {
		t := &tmpls[s]
		var se float64
		for i := 0; i < 64; i++ {
			d := blk[i] - t[i]
			se += d * d
		}
		if se < bestErr {
			bestErr = se
			bestSym = s
		}
	}
	return bestSym
}

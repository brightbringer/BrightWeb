package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ytcloud/internal/media"
)

// labPattern defines a visual/geometric modulation scheme for empirical benchmarking.
type labPattern struct {
	Code        string
	Title       string
	CellW       int
	CellH       int
	BitsPerCell int // integer bits per cell (or -25 for 2.5 bits/cell y6)
	Templates   [][]float64
	EncodeCell  func(Y []byte, stride, x0, y0, sym int, frameIdx int, seed uint64)
	DecodeCell  func(Y []byte, stride, x0, y0 int, frameIdx int, seed uint64) int
}

// buildTemplateBank precomputes normalized matched-filter templates (or L2 prototype patches)
// for a CellW x CellH pattern with N symbols.
func makeTemplatePattern(code, title string, sz, bits int, gen func(sym, x, y int) float64) labPattern {
	nSym := 1 << bits
	tmpls := make([][]float64, nSym)
	bytePatches := make([][]byte, nSym)
	for s := 0; s < nSym; s++ {
		t := make([]float64, sz*sz)
		bp := make([]byte, sz*sz)
		for y := 0; y < sz; y++ {
			for x := 0; x < sz; x++ {
				v := gen(s, x, y)
				if v < 16 {
					v = 16
				} else if v > 240 {
					v = 240
				}
				t[y*sz+x] = v
				bp[y*sz+x] = byte(math.Round(v))
			}
		}
		tmpls[s] = t
		bytePatches[s] = bp
	}

	return labPattern{
		Code:        code,
		Title:       title,
		CellW:       sz,
		CellH:       sz,
		BitsPerCell: bits,
		Templates:   tmpls,
		EncodeCell: func(Y []byte, stride, x0, y0, sym int, _ int, _ uint64) {
			bp := bytePatches[sym]
			for y := 0; y < sz; y++ {
				rowOff := (y0+y)*stride + x0
				copy(Y[rowOff:rowOff+sz], bp[y*sz:(y+1)*sz])
			}
		},
		DecodeCell: func(Y []byte, stride, x0, y0 int, _ int, _ uint64) int {
			bestSym := 0
			bestErr := math.MaxFloat64
			for s := 0; s < nSym; s++ {
				t := tmpls[s]
				var se float64
				for y := 0; y < sz; y++ {
					rowOff := (y0+y)*stride + x0
					tOff := y * sz
					for x := 0; x < sz; x++ {
						d := float64(Y[rowOff+x]) - t[tOff+x]
						se += d * d
					}
				}
				if se < bestErr {
					bestErr = se
					bestSym = s
				}
			}
			return bestSym
		},
	}
}

func buildAllLabPatterns() []labPattern {
	// 1. Fractal Chroma + 6-Gray (baseline artistic compact)
	y6Levels := [6]byte{24, 65, 107, 148, 190, 231}
	pFractalY6 := labPattern{
		Code:        "fractal-y6",
		Title:       "1. Fractal Chroma + y6 (8x8, 2.5b)",
		CellW:       8,
		CellH:       8,
		BitsPerCell: -25, // special marker for 2.5 bits/cell (5 bits per 2 cells)
		EncodeCell: func(Y []byte, stride, x0, y0, sym int, _ int, _ uint64) {
			v := y6Levels[sym]
			for y := 0; y < 8; y++ {
				rowOff := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					Y[rowOff+x] = v
				}
			}
		},
		DecodeCell: func(Y []byte, stride, x0, y0 int, _ int, _ uint64) int {
			var sum int
			for y := 2; y < 6; y++ {
				rowOff := (y0+y)*stride + x0
				for x := 2; x < 6; x++ {
					sum += int(Y[rowOff+x])
				}
			}
			avg := sum / 16
			best, bestD := 0, 999
			for k := 0; k < 6; k++ {
				d := avg - int(y6Levels[k])
				if d < 0 {
					d = -d
				}
				if d < bestD {
					bestD = d
					best = k
				}
			}
			return best
		},
	}

	// 2. 3D Fractal Relief Subtractive (8x8, 2 bits/cell = 2x denser than art-stego!)
	// Uses deterministic 3D Fractal Dream luminance [68..188] + 4-level Gray-coded offset {-48, -16, +16, +48}.
	fractalBaseY := func(x0, y0, frameIdx int, seed uint64) float64 {
		s32 := uint32(seed ^ (seed >> 32))
		t := float64(frameIdx) * 0.045
		nx := (float64(x0+4) / 2560.0) * 3.4 * 1.777
		ny := (float64(y0+4) / 1440.0) * 3.4
		qx := fractalFBM3(nx+t*0.5, ny-t*0.35, s32)
		qy := fractalFBM3(nx+5.2-t*0.4, ny+1.3+t*0.55, s32+101)
		r := fractalFBM3(nx+2.2*qx+1.7, ny+2.2*qy+9.2, s32+211)
		c := math.Max(0, math.Min(1, (r-0.18)*1.55))
		return 68.0 + c*120.0
	}
	reliefOffsets := [4]float64{-48, -16, 16, 48}
	gray2Enc := [4]int{0, 1, 3, 2}
	gray2Dec := [4]int{0, 1, 3, 2}
	pFractalRelief := labPattern{
		Code:        "fractal-3d-y4",
		Title:       "2. Fractal 3D Relevo (8x8, 2b)",
		CellW:       8,
		CellH:       8,
		BitsPerCell: 2,
		EncodeCell: func(Y []byte, stride, x0, y0, sym int, frameIdx int, seed uint64) {
			base := fractalBaseY(x0, y0, frameIdx, seed)
			v := clampYUV(int32(math.Round(base+reliefOffsets[gray2Enc[sym]])), 16, 240)
			for y := 0; y < 8; y++ {
				rowOff := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					Y[rowOff+x] = v
				}
			}
		},
		DecodeCell: func(Y []byte, stride, x0, y0 int, frameIdx int, seed uint64) int {
			var sum float64
			for y := 2; y < 6; y++ {
				rowOff := (y0+y)*stride + x0
				for x := 2; x < 6; x++ {
					sum += float64(Y[rowOff+x])
				}
			}
			diff := sum/16.0 - fractalBaseY(x0, y0, frameIdx, seed)
			bestIdx := 0
			bestD := math.Abs(diff - reliefOffsets[0])
			for k := 1; k < 4; k++ {
				d := math.Abs(diff - reliefOffsets[k])
				if d < bestD {
					bestD = d
					bestIdx = k
				}
			}
			return gray2Dec[bestIdx]
		},
	}

	// 3. Lines & Angles 8x8: 4 orthogonal angles (0, 45, 90, 135 deg) x 2 DC levels = 3 bits/cell
	pAngles8 := makeTemplatePattern("angles-8x8", "3. Linhas & Angulos 4-Dir (8x8, 3b)", 8, 3, func(sym, x, y int) float64 {
		angIdx := sym & 3
		dc := 84.0
		if (sym>>2)&1 == 1 {
			dc = 172.0
		}
		fx := float64(x) + 0.5
		fy := float64(y) + 0.5
		var phase float64
		switch angIdx {
		case 0: // 0 deg (horizontal lines)
			phase = 2.0 * math.Pi * fy / 8.0
		case 1: // 90 deg (vertical lines)
			phase = 2.0 * math.Pi * fx / 8.0
		case 2: // 45 deg (\ diagonal)
			phase = 2.0 * math.Pi * (fx - fy) / 8.0
		case 3: // 135 deg (/ diagonal)
			phase = 2.0 * math.Pi * (fx + fy) / 8.0
		}
		return dc + 58.0*math.Cos(phase)
	})

	// 4. Brushstrokes / Flow Field 12x12: 8 angles (0..157.5 deg) x 2 DC levels = 4 bits/cell
	pAngles12 := makeTemplatePattern("angles-12-8dir", "4. Campo de Fluxo 8-Ang (12x12, 4b)", 12, 4, func(sym, x, y int) float64 {
		angIdx := sym & 7
		dc := 82.0
		if (sym>>3)&1 == 1 {
			dc = 174.0
		}
		theta := float64(angIdx) * math.Pi / 8.0 // 0, 22.5, 45, ..., 157.5 deg
		cx := float64(x) - 5.5
		cy := float64(y) - 5.5
		// Projection perpendicular to stroke direction
		u := -cx*math.Sin(theta) + cy*math.Cos(theta)
		// Soft Gaussian-windowed Gabor ridge (smooth brushstroke!)
		wave := math.Cos(2.0 * math.Pi * u / 8.5)
		return dc + 62.0*wave
	})

	// 5. Continuous Truchet Curves 12x12: 4 seamless edge-midpoint topologies x 2 brightness tiers = 3 bits/cell
	// Topologies all enter/leave exact edge midpoints (5.5, 0), (11, 5.5), (5.5, 11), (0, 5.5):
	//   0: TL + BR quarter-circle arcs
	//   1: TR + BL quarter-circle arcs
	//   2: H + V smooth woven cross
	//   3: Diamond lozenge connecting all 4 midpoints
	pTruchet12 := makeTemplatePattern("truchet-12", "5. Curvas Truchet (12x12, 3b)", 12, 3, func(sym, x, y int) float64 {
		topo := sym & 3
		tier := (sym >> 2) & 1
		fx := float64(x) + 0.5
		fy := float64(y) + 0.5
		s := 12.0
		r := s * 0.5 // 6.0
		var dist float64
		switch topo {
		case 0: // arcs centered at (0,0) and (s,s)
			d1 := math.Abs(math.Hypot(fx, fy) - r)
			d2 := math.Abs(math.Hypot(fx-s, fy-s) - r)
			dist = math.Min(d1, d2)
		case 1: // arcs centered at (s,0) and (0,s)
			d1 := math.Abs(math.Hypot(fx-s, fy) - r)
			d2 := math.Abs(math.Hypot(fx, fy-s) - r)
			dist = math.Min(d1, d2)
		case 2: // cross: horizontal & vertical bands through center
			d1 := math.Abs(fx - r)
			d2 := math.Abs(fy - r)
			dist = math.Min(d1, d2)
		case 3: // diamond ring connecting midpoints (|x-6|+|y-6| = 6)
			dist = math.Abs((math.Abs(fx-r)+math.Abs(fy-r))-r) * 0.7071
		}
		// Smooth anti-aliased ribbon of half-width ~1.7px
		ribbon := math.Max(0, math.Min(1, (2.1-dist)/1.2))
		if tier == 0 {
			// Deep obsidian ink with warm silver-white glowing arcs
			return 28.0 + ribbon*136.0
		}
		// Soft misty canvas with bright luminous arcs (keeps curve flow legible!)
		return 108.0 + ribbon*124.0
	})

	// 6. Bauhaus Diagonal Triangles / Stained Glass 12x12: 4 cuts (\, /, H, V) x 2 polarities = 3 bits/cell
	pBauhaus12 := makeTemplatePattern("bauhaus-12", "6. Vitral Bauhaus (12x12, 3b)", 12, 3, func(sym, x, y int) float64 {
		cut := sym & 3
		pol := (sym >> 2) & 1
		cx := float64(x) - 5.5
		cy := float64(y) - 5.5
		var signed float64
		switch cut {
		case 0: // \ diagonal
			signed = cx - cy
		case 1: // / diagonal
			signed = cx + cy
		case 2: // horizontal split
			signed = cy
		case 3: // vertical split
			signed = cx
		}
		if pol == 1 {
			signed = -signed
		}
		// Crisp anti-aliased bevel
		alpha := math.Max(0, math.Min(1, 0.5+signed*0.45))
		return 34.0 + alpha*188.0
	})

	// 7. Constellation / Bokeh Orbs 12x12: 4 quadrant positions x 2 polarities = 3 bits/cell
	pConstel12 := makeTemplatePattern("constel-12", "7. Constelacao Bokeh (12x12, 3b)", 12, 3, func(sym, x, y int) float64 {
		quad := sym & 3
		pol := (sym >> 2) & 1
		ox := 3.2
		if quad&1 == 1 {
			ox = 8.8
		}
		oy := 3.2
		if (quad>>1)&1 == 1 {
			oy = 8.8
		}
		d := math.Hypot(float64(x)+0.5-ox, float64(y)+0.5-oy)
		orb := math.Max(0, math.Min(1, (3.8-d)/1.6))
		if pol == 0 {
			return 26.0 + orb*202.0 // bright star on dark night
		}
		return 228.0 - orb*202.0 // dark moon on bright glow
	})

	// 8. DC-Preserving Halftone Dots 8x8 (3 bits/cell = 8 exact DC mean levels rendered as smooth halftone circles!)
	// Visually looks like magazine halftone / organic dots, yet decodes purely from the 8x8 DC average!
	var htLevels [8]float64
	var htPatches [8][64]byte
	gray3Enc := [8]int{0, 1, 3, 2, 6, 7, 5, 4}
	gray3Dec := [8]int{0, 1, 3, 2, 7, 6, 4, 5}
	for lv := 0; lv < 8; lv++ {
		targetMean := 26.0 + float64(lv)*(204.0/7.0)
		htLevels[lv] = targetMean
		// Build a smooth radial halftone dot and shift/scale slightly so exact mean == targetMean!
		// Lower levels: small bright dot on dark bg; higher levels: large glowing orb filling cell.
		radius := 0.8 + float64(lv)*0.68
		var raw [64]float64
		var sum float64
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				d := math.Hypot(float64(x)-3.5, float64(y)-3.5)
				blob := math.Max(0, math.Min(1, (radius-d)/1.4))
				v := 20.0 + blob*216.0
				raw[y*8+x] = v
				sum += v
			}
		}
		curMean := sum / 64.0
		shift := targetMean - curMean
		for i := 0; i < 64; i++ {
			v := raw[i] + shift
			if v < 16 {
				v = 16
			} else if v > 240 {
				v = 240
			}
			htPatches[lv][i] = byte(math.Round(v))
		}
		// Recompute actual exact mean after clamping
		var actual float64
		for i := 0; i < 64; i++ {
			actual += float64(htPatches[lv][i])
		}
		htLevels[lv] = actual / 64.0
	}
	pHalftoneDC8 := labPattern{
		Code:        "halftone-dc-8",
		Title:       "8. Halftone DC-Suave (8x8, 3b)",
		CellW:       8,
		CellH:       8,
		BitsPerCell: 3,
		EncodeCell: func(Y []byte, stride, x0, y0, sym int, _ int, _ uint64) {
			lv := gray3Enc[sym]
			p := htPatches[lv]
			for y := 0; y < 8; y++ {
				rowOff := (y0+y)*stride + x0
				copy(Y[rowOff:rowOff+8], p[y*8:(y+1)*8])
			}
		},
		DecodeCell: func(Y []byte, stride, x0, y0 int, _ int, _ uint64) int {
			var sum float64
			for y := 0; y < 8; y++ {
				rowOff := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					sum += float64(Y[rowOff+x])
				}
			}
			avg := sum / 64.0
			bestLv := 0
			bestD := math.Abs(avg - htLevels[0])
			for lv := 1; lv < 8; lv++ {
				d := math.Abs(avg - htLevels[lv])
				if d < bestD {
					bestD = d
					bestLv = lv
				}
			}
			return gray3Dec[bestLv]
		},
	}

	return []labPattern{
		pFractalY6,
		pFractalRelief,
		pAngles8,
		pAngles12,
		pTruchet12,
		pBauhaus12,
		pConstel12,
		pHalftoneDC8,
	}
}

func (p *labPattern) frameCapacities(w, h int) (cols, rows, rawBytes, payloadBytes int) {
	const margin = 16
	cols = (w - 2*margin) / p.CellW
	rows = (h - 2*margin) / p.CellH
	nCells := cols * rows
	var totalBits int
	if p.BitsPerCell == -25 {
		totalBits = (nCells / 2) * 5
	} else {
		totalBits = nCells * p.BitsPerCell
	}
	rawBytes = totalBits / 8
	numRS := rawBytes / 255
	payloadBytes = numRS * 191 // RS(255, 191) = 25% FEC
	return cols, rows, rawBytes, payloadBytes
}

func (p *labPattern) encodeFrame(dst *YUVFrame, payloadChunk []byte, frameIdx int, seed uint64) {
	cols, rows, rawBytes, _ := p.frameCapacities(dst.Width, dst.Height)
	rs := GetRSCodec(191)
	raw := rs.EncodeInterleaved(payloadChunk, (rawBytes/255)*255)
	// Pad remaining tail bytes deterministically
	if len(raw) < rawBytes {
		pad := make([]byte, rawBytes)
		copy(pad, raw)
		raw = pad
	}

	// Render smooth Fractal Dream color in Cb/Cr (looks gorgeous, costs almost zero bitrate!)
	U, V := RenderFractalChromaFields(dst.Width/2, dst.Height/2, frameIdx, seed, 48, 208)
	copy(dst.UPlane(), U)
	copy(dst.VPlane(), V)

	// Neutral margin border
	Y := dst.YPlane()
	for i := range Y {
		Y[i] = 28
	}

	const margin = 16
	bitPos := 0
	readBits := func(n int) int {
		v := 0
		for i := 0; i < n; i++ {
			bIdx := bitPos >> 3
			bShift := uint(7 - (bitPos & 7))
			bit := 0
			if bIdx < len(raw) {
				bit = int((raw[bIdx] >> bShift) & 1)
			}
			v = (v << 1) | bit
			bitPos++
		}
		return v
	}

	if p.BitsPerCell == -25 {
		// 5 bits per pair of cells -> s0 = v/6, s1 = v%6 (v in 0..31)
		nCells := cols * rows
		for c := 0; c+1 < nCells; c += 2 {
			v := readBits(5)
			s0, s1 := v/6, v%6
			r0, c0 := c/cols, c%cols
			r1, c1 := (c+1)/cols, (c+1)%cols
			p.EncodeCell(Y, dst.Width, margin+c0*p.CellW, margin+r0*p.CellH, s0, frameIdx, seed)
			p.EncodeCell(Y, dst.Width, margin+c1*p.CellW, margin+r1*p.CellH, s1, frameIdx, seed)
		}
		return
	}

	for r := 0; r < rows; r++ {
		y0 := margin + r*p.CellH
		for c := 0; c < cols; c++ {
			sym := readBits(p.BitsPerCell)
			p.EncodeCell(Y, dst.Width, margin+c*p.CellW, y0, sym, frameIdx, seed)
		}
	}
}

func (p *labPattern) decodeFrame(src *YUVFrame, frameIdx int, seed uint64) ([]byte, int, error) {
	cols, rows, rawBytes, _ := p.frameCapacities(src.Width, src.Height)
	raw := make([]byte, rawBytes)
	Y := src.YPlane()
	const margin = 16

	bitPos := 0
	writeBits := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bIdx := bitPos >> 3
			bShift := uint(7 - (bitPos & 7))
			if bIdx < len(raw) && ((v>>uint(i))&1) != 0 {
				raw[bIdx] |= 1 << bShift
			}
			bitPos++
		}
	}

	if p.BitsPerCell == -25 {
		nCells := cols * rows
		for c := 0; c+1 < nCells; c += 2 {
			r0, c0 := c/cols, c%cols
			r1, c1 := (c+1)/cols, (c+1)%cols
			s0 := p.DecodeCell(Y, src.Width, margin+c0*p.CellW, margin+r0*p.CellH, frameIdx, seed)
			s1 := p.DecodeCell(Y, src.Width, margin+c1*p.CellW, margin+r1*p.CellH, frameIdx, seed)
			v := s0*6 + s1
			if v > 31 {
				v = 31
			}
			writeBits(v, 5)
		}
	} else {
		for r := 0; r < rows; r++ {
			y0 := margin + r*p.CellH
			for c := 0; c < cols; c++ {
				sym := p.DecodeCell(Y, src.Width, margin+c*p.CellW, y0, frameIdx, seed)
				writeBits(sym, p.BitsPerCell)
			}
		}
	}

	rs := GetRSCodec(191)
	return rs.DecodeInterleaved(raw[:(rawBytes/255)*255])
}

func yuvCropToRGBA(f *YUVFrame, x0, y0, cw, ch int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, cw, ch))
	Y := f.YPlane()
	U := f.UPlane()
	V := f.VPlane()
	uvW := f.Width / 2
	for y := 0; y < ch; y++ {
		sy := y0 + y
		for x := 0; x < cw; x++ {
			sx := x0 + x
			yv := float64(Y[sy*f.Width+sx])
			uvIdx := (sy/2)*uvW + (sx / 2)
			cb := float64(U[uvIdx]) - 128.0
			cr := float64(V[uvIdx]) - 128.0
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
			img.SetRGBA(x, y, color.RGBA{R: clampB(r), G: clampB(g), B: clampB(b), A: 255})
		}
	}
	return img
}

// Tiny 5x7 uppercase/ASCII bitmap font for clean PNG panel titles
var miniFont = map[rune][7]string{
	'A': {"01110", "10001", "10001", "11111", "10001", "10001", "10001"},
	'B': {"11110", "10001", "10001", "11110", "10001", "10001", "11110"},
	'C': {"01111", "10000", "10000", "10000", "10000", "10000", "01111"},
	'D': {"11110", "10001", "10001", "10001", "10001", "10001", "11110"},
	'E': {"11111", "10000", "10000", "11110", "10000", "10000", "11111"},
	'F': {"11111", "10000", "10000", "11110", "10000", "10000", "10000"},
	'G': {"01111", "10000", "10000", "10111", "10001", "10001", "01111"},
	'H': {"10001", "10001", "10001", "11111", "10001", "10001", "10001"},
	'I': {"11111", "00100", "00100", "00100", "00100", "00100", "11111"},
	'J': {"00111", "00010", "00010", "00010", "10010", "10010", "01100"},
	'K': {"10001", "10010", "10100", "11000", "10100", "10010", "10001"},
	'L': {"10000", "10000", "10000", "10000", "10000", "10000", "11111"},
	'M': {"10001", "11011", "10101", "10001", "10001", "10001", "10001"},
	'N': {"10001", "11001", "10101", "10011", "10001", "10001", "10001"},
	'O': {"01110", "10001", "10001", "10001", "10001", "10001", "01110"},
	'P': {"11110", "10001", "10001", "11110", "10000", "10000", "10000"},
	'Q': {"01110", "10001", "10001", "10001", "10101", "10010", "01101"},
	'R': {"11110", "10001", "10001", "11110", "10100", "10010", "10001"},
	'S': {"01111", "10000", "10000", "01110", "00001", "00001", "11110"},
	'T': {"11111", "00100", "00100", "00100", "00100", "00100", "00100"},
	'U': {"10001", "10001", "10001", "10001", "10001", "10001", "01110"},
	'V': {"10001", "10001", "10001", "10001", "10001", "01010", "00100"},
	'W': {"10001", "10001", "10001", "10001", "10101", "11011", "10001"},
	'X': {"10001", "10001", "01010", "00100", "01010", "10001", "10001"},
	'Y': {"10001", "10001", "01010", "00100", "00100", "00100", "00100"},
	'Z': {"11111", "00001", "00010", "00100", "01000", "10000", "11111"},
	'0': {"01110", "10011", "10101", "10101", "11001", "10001", "01110"},
	'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	'2': {"01110", "10001", "00001", "00110", "01000", "10000", "11111"},
	'3': {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5': {"11111", "10000", "11110", "00001", "00001", "10001", "01110"},
	'6': {"01110", "10000", "11110", "10001", "10001", "10001", "01110"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8': {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00001", "01110"},
	'.': {"00000", "00000", "00000", "00000", "00000", "01100", "01100"},
	'-': {"00000", "00000", "00000", "11111", "00000", "00000", "00000"},
	'+': {"00000", "00100", "00100", "11111", "00100", "00100", "00000"},
	'&': {"01100", "10010", "10100", "01000", "10101", "10010", "01101"},
	'(': {"00010", "00100", "01000", "01000", "01000", "00100", "00010"},
	')': {"01000", "00100", "00010", "00010", "00010", "00100", "01000"},
	' ': {"00000", "00000", "00000", "00000", "00000", "00000", "00000"},
}

func drawText(img *image.RGBA, x0, y0, scale int, text string, col color.RGBA) {
	cx := x0
	for _, ch := range text {
		if ch >= 'a' && ch <= 'z' {
			ch -= 32
		}
		glyph, ok := miniFont[ch]
		if !ok {
			glyph = miniFont[' ']
		}
		for gy := 0; gy < 7; gy++ {
			for gx := 0; gx < 5; gx++ {
				if glyph[gy][gx] == '1' {
					for sy := 0; sy < scale; sy++ {
						for sx := 0; sx < scale; sx++ {
							img.SetRGBA(cx+gx*scale+sx, y0+gy*scale+sy, col)
						}
					}
				}
			}
		}
		cx += 6 * scale
	}
}

func runFFmpegEncodeRaw(ffmpegPath string, w, h, fps int, frames []*YUVFrame, extraArgs []string, outPath string) (int64, time.Duration, error) {
	t0 := time.Now()
	args := []string{
		"-y", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", "yuv420p",
		"-s:v", fmt.Sprintf("%dx%d", w, h),
		"-r", fmt.Sprintf("%d", fps),
		"-i", "pipe:0",
	}
	args = append(args, extraArgs...)
	args = append(args, "-pix_fmt", "yuv420p", outPath)

	cmd := exec.Command(ffmpegPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, 0, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return 0, 0, err
	}
	for _, f := range frames {
		if _, err := stdin.Write(f.Buf); err != nil {
			_ = stdin.Close()
			_ = cmd.Wait()
			return 0, 0, fmt.Errorf("stdin write: %w (%s)", err, stderr.String())
		}
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		return 0, 0, fmt.Errorf("ffmpeg encode: %w (%s)", err, stderr.String())
	}
	st, err := os.Stat(outPath)
	if err != nil {
		return 0, 0, err
	}
	return st.Size(), time.Since(t0), nil
}

func runFFmpegTranscode(ffmpegPath, inPath, outPath string, extraArgs []string) (int64, error) {
	args := []string{"-y", "-loglevel", "error", "-i", inPath}
	args = append(args, extraArgs...)
	args = append(args, "-pix_fmt", "yuv420p", outPath)
	cmd := exec.Command(ffmpegPath, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("transcode: %w (%s)", err, string(out))
	}
	st, err := os.Stat(outPath)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func decodeVideoAndVerify(ffmpegPath, vidPath string, w, h int, p *labPattern, origPayload []byte, seed uint64) (okFrames, totalFrames, totalCorrected int, exactMatch bool) {
	_, _, _, capPerFrame := p.frameCapacities(w, h)
	totalFrames = (len(origPayload) + capPerFrame - 1) / capPerFrame

	cmd := exec.Command(ffmpegPath,
		"-loglevel", "error",
		"-i", vidPath,
		"-f", "rawvideo", "-pix_fmt", "yuv420p",
		"pipe:1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, totalFrames, 0, false
	}
	if err := cmd.Start(); err != nil {
		return 0, totalFrames, 0, false
	}

	frameSize := w * h * 3 / 2
	buf := make([]byte, frameSize)
	recovered := make([]byte, 0, len(origPayload))
	for fi := 0; fi < totalFrames; fi++ {
		if _, err := io.ReadFull(stdout, buf); err != nil {
			break
		}
		f := &YUVFrame{Width: w, Height: h, Buf: buf}
		chunk, corr, err := p.decodeFrame(f, fi, seed)
		if err != nil {
			continue
		}
		okFrames++
		totalCorrected += corr
		off := fi * capPerFrame
		need := len(origPayload) - off
		if need > capPerFrame {
			need = capPerFrame
		}
		recovered = append(recovered, chunk[:need]...)
	}
	_ = cmd.Wait()
	exactMatch = okFrames == totalFrames && len(recovered) == len(origPayload) && bytes.Equal(recovered, origPayload)
	return okFrames, totalFrames, totalCorrected, exactMatch
}

func TestArtisticPatternLab(t *testing.T) {
	if os.Getenv("YTCLOUD_PATTERN_LAB") != "1" {
		t.Skip("set YTCLOUD_PATTERN_LAB=1 to run the artistic/geometric pattern benchmark")
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
		seed       = uint64(0xA171571CF1AC7A1)
	)

	payload := make([]byte, payloadLen)
	rng := rand.New(rand.NewSource(20261123))
	rng.Read(payload)
	// Avoid unused import warning for binary
	_ = binary.LittleEndian

	patterns := buildAllLabPatterns()
	dir := t.TempDir()

	// Visual showcase panel: 4 columns x 2 rows of 384x340 tiles (+ 36px header bar per tile)
	const (
		tileW = 384
		tileH = 340
		hdrH  = 34
		pad   = 14
		colsG = 4
		rowsG = 2
	)
	panelW := pad + colsG*(tileW+pad)
	panelH := pad + rowsG*(tileH+hdrH+pad)
	panel := image.NewRGBA(image.Rect(0, 0, panelW, panelH))
	for i := 0; i < len(panel.Pix); i += 4 {
		panel.Pix[i], panel.Pix[i+1], panel.Pix[i+2], panel.Pix[i+3] = 16, 18, 26, 255
	}

	t.Logf("=== ARTISTIC & GEOMETRIC PATTERN LAB (2K 2560x1440 @ 15fps | 128 KB incompressible payload | RS 25%%) ===")
	t.Logf("%-16s | %4s | %3s | %-11s | %-11s | %-14s | %-14s",
		"Pattern", "B/f", "Frm", "H.264 Up", "AV1 Up", "YT VP9 (fromH)", "YT VP9 (fromA)")

	for idx := range patterns {
		p := &patterns[idx]
		_, _, _, capPerFrame := p.frameCapacities(W, H)
		nFrames := (payloadLen + capPerFrame - 1) / capPerFrame
		frames := make([]*YUVFrame, nFrames)
		for fi := 0; fi < nFrames; fi++ {
			f := NewYUVFrame(W, H)
			off := fi * capPerFrame
			end := off + capPerFrame
			if end > payloadLen {
				end = payloadLen
			}
			p.encodeFrame(f, payload[off:end], fi, seed)
			frames[fi] = f
		}

		// Sanity-check lossless in-memory roundtrip first!
		if chunk, corr, err := p.decodeFrame(frames[0], 0, seed); err != nil || corr != 0 || !bytes.Equal(chunk[:minInt(capPerFrame, payloadLen)], payload[:minInt(capPerFrame, payloadLen)]) {
			t.Fatalf("%s lossless in-memory roundtrip failed: err=%v corr=%d", p.Code, err, corr)
		}

		// Paste a crisp native-pixel crop of frame 0 into the 4x2 showcase PNG!
		crop := yuvCropToRGBA(frames[0], 640, 360, tileW, tileH)
		gc := idx % colsG
		gr := idx / colsG
		px0 := pad + gc*(tileW+pad)
		py0 := pad + gr*(tileH+hdrH+pad)
		// Header bar
		for y := 0; y < hdrH; y++ {
			for x := 0; x < tileW; x++ {
				panel.SetRGBA(px0+x, py0+y, color.RGBA{R: 28, G: 34, B: 48, A: 255})
			}
		}
		drawText(panel, px0+8, py0+10, 2, p.Title, color.RGBA{R: 235, G: 242, B: 255, A: 255})
		for y := 0; y < tileH; y++ {
			for x := 0; x < tileW; x++ {
				panel.SetRGBA(px0+x, py0+hdrH+y, crop.RGBAAt(x, y))
			}
		}

		// Test 1: H.264 medium (CRF 30 for geometric/high-detail, CRF 34 for y6)
		h264CRF := "30"
		av1CRF := "40"
		if p.Code == "fractal-y6" {
			h264CRF = "34"
			av1CRF = "46"
		} else if p.Code == "halftone-dc-8" {
			h264CRF = "30"
			av1CRF = "42"
		}

		h264Path := filepath.Join(dir, p.Code+"_h264.mp4")
		hSize, hDur, err := runFFmpegEncodeRaw(tools.FFmpegPath, W, H, FPS, frames,
			[]string{"-c:v", "libx264", "-preset", "medium", "-crf", h264CRF}, h264Path)
		if err != nil {
			t.Fatalf("%s h264 encode: %v", p.Code, err)
		}
		hOk, hTot, hCorr, hMatch := decodeVideoAndVerify(tools.FFmpegPath, h264Path, W, H, p, payload, seed)

		// Test 2: AV1 realtime
		av1Path := filepath.Join(dir, p.Code+"_av1.mp4")
		aSize, aDur, err := runFFmpegEncodeRaw(tools.FFmpegPath, W, H, FPS, frames,
			[]string{"-c:v", "libaom-av1", "-usage", "realtime", "-cpu-used", "8", "-crf", av1CRF, "-b:v", "0", "-row-mt", "1", "-tiles", "2x2"}, av1Path)
		if err != nil {
			t.Fatalf("%s av1 encode: %v", p.Code, err)
		}
		aOk, aTot, aCorr, aMatch := decodeVideoAndVerify(tools.FFmpegPath, av1Path, W, H, p, payload, seed)

		// Test 3: Simulated YouTube 2K VP9 (-deadline good -cpu-used 5 -b:v 10M) from BOTH masters!
		vp9FromHPath := filepath.Join(dir, p.Code+"_vp9_fromH.webm")
		vp9HSize, err := runFFmpegTranscode(tools.FFmpegPath, h264Path, vp9FromHPath,
			[]string{"-c:v", "libvpx-vp9", "-deadline", "good", "-cpu-used", "5", "-b:v", "10M", "-row-mt", "1"})
		if err != nil {
			t.Fatalf("%s vp9 from h264: %v", p.Code, err)
		}
		vhOk, vhTot, vhCorr, vhMatch := decodeVideoAndVerify(tools.FFmpegPath, vp9FromHPath, W, H, p, payload, seed)

		vp9FromAPath := filepath.Join(dir, p.Code+"_vp9_fromA.webm")
		vp9ASize, err := runFFmpegTranscode(tools.FFmpegPath, av1Path, vp9FromAPath,
			[]string{"-c:v", "libvpx-vp9", "-deadline", "good", "-cpu-used", "5", "-b:v", "10M", "-row-mt", "1"})
		if err != nil {
			t.Fatalf("%s vp9 from av1: %v", p.Code, err)
		}
		vaOk, vaTot, vaCorr, vaMatch := decodeVideoAndVerify(tools.FFmpegPath, vp9FromAPath, W, H, p, payload, seed)

		fmtStatus := func(sz int64, ok, tot, corr int, match bool, dt time.Duration) string {
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

		t.Logf("%-16s | %4d | %3d | %-18s | %-18s | %-15s | %-15s",
			p.Code, capPerFrame, nFrames,
			fmtStatus(hSize, hOk, hTot, hCorr, hMatch, hDur),
			fmtStatus(aSize, aOk, aTot, aCorr, aMatch, aDur),
			fmtStatus(vp9HSize, vhOk, vhTot, vhCorr, vhMatch, 0),
			fmtStatus(vp9ASize, vaOk, vaTot, vaCorr, vaMatch, 0),
		)
	}

	// Save visual preview PNG to artifact dir if requested via env var
	if outPNG := os.Getenv("YTCLOUD_PATTERN_PNG"); outPNG != "" {
		f, err := os.Create(outPNG)
		if err != nil {
			t.Fatalf("create preview PNG: %v", err)
		}
		defer f.Close()
		if err := png.Encode(f, panel); err != nil {
			t.Fatalf("encode preview PNG: %v", err)
		}
		t.Logf("Saved visual preview panel: %s (%dx%d)", outPNG, panelW, panelH)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestArtisticPatternLabRound2 tests 8x8 DCT-aligned, amplitude-calibrated versions of the winning
// artistic & geometric patterns (Lines/Angles, Bauhaus, Truchet, 3D Fractal Relief, and Fractal+Angles)
// to measure the true minimum upload/download ratios in H.264, AV1, and YouTube VP9.
func TestArtisticPatternLabRound2(t *testing.T) {
	if os.Getenv("YTCLOUD_PATTERN_LAB") != "2" {
		t.Skip("set YTCLOUD_PATTERN_LAB=2 to run round 2 (8x8 DCT-aligned optimization)")
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
		seed       = uint64(0xA171571CF1AC7A1)
	)
	payload := make([]byte, payloadLen)
	rand.New(rand.NewSource(20261123)).Read(payload)

	// Shared 3D Fractal luminance helper
	fractalBaseY := func(x0, y0, frameIdx int, s uint64) float64 {
		s32 := uint32(s ^ (s >> 32))
		tm := float64(frameIdx) * 0.045
		nx := (float64(x0+4) / 2560.0) * 3.4 * 1.777
		ny := (float64(y0+4) / 1440.0) * 3.4
		qx := fractalFBM3(nx+tm*0.5, ny-tm*0.35, s32)
		qy := fractalFBM3(nx+5.2-tm*0.4, ny+1.3+tm*0.55, s32+101)
		r := fractalFBM3(nx+2.2*qx+1.7, ny+2.2*qy+9.2, s32+211)
		c := math.Max(0, math.Min(1, (r-0.18)*1.55))
		return 64.0 + c*128.0
	}

	// 1. Fractal 3D Relevo (8x8, 2b, calibrated ±42/±14)
	relOff := [4]float64{-42, -14, 14, 42}
	g2e := [4]int{0, 1, 3, 2}
	g2d := [4]int{0, 1, 3, 2}
	pFractal3D := labPattern{
		Code: "fractal-3d-opt", Title: "1. Fractal 3D Relevo (8x8 2b)",
		CellW: 8, CellH: 8, BitsPerCell: 2,
		EncodeCell: func(Y []byte, stride, x0, y0, sym, fi int, s uint64) {
			v := clampYUV(int32(math.Round(fractalBaseY(x0, y0, fi, s)+relOff[g2e[sym]])), 16, 240)
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					Y[row+x] = v
				}
			}
		},
		DecodeCell: func(Y []byte, stride, x0, y0, fi int, s uint64) int {
			var sum float64
			for y := 2; y < 6; y++ {
				row := (y0+y)*stride + x0
				for x := 2; x < 6; x++ {
					sum += float64(Y[row+x])
				}
			}
			diff := sum/16.0 - fractalBaseY(x0, y0, fi, s)
			best, bestD := 0, math.Abs(diff-relOff[0])
			for k := 1; k < 4; k++ {
				if d := math.Abs(diff - relOff[k]); d < bestD {
					bestD, best = d, k
				}
			}
			return g2d[best]
		},
	}

	// 2. Linhas & Angulos 8x8 Otimizado (4 angles x 2 DC levels = 3b/cell, softer amp ±38)
	pAngles8Opt := makeTemplatePattern("angles-8x8-opt", "2. Linhas & Angulos Opt (8x8 3b)", 8, 3, func(sym, x, y int) float64 {
		angIdx := sym & 3
		dc := 88.0
		if (sym>>2)&1 == 1 {
			dc = 168.0
		}
		fx, fy := float64(x)+0.5, float64(y)+0.5
		var ph float64
		switch angIdx {
		case 0:
			ph = 2.0 * math.Pi * fy / 8.0
		case 1:
			ph = 2.0 * math.Pi * fx / 8.0
		case 2:
			ph = 2.0 * math.Pi * (fx - fy) / 8.0
		case 3:
			ph = 2.0 * math.Pi * (fx + fy) / 8.0
		}
		return dc + 38.0*math.Cos(ph)
	})

	// 3. Fractal 3D + Linhas/Angulos (8x8, 2b = 4 orthogonal zero-mean angles etched over 3D Fractal!)
	// Blind angle decoding via inner product with the 4 orthogonal zero-mean 8x8 wave bases!
	var angBases [4][64]float64
	for a := 0; a < 4; a++ {
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				fx, fy := float64(x)+0.5, float64(y)+0.5
				var ph float64
				switch a {
				case 0:
					ph = 2.0 * math.Pi * fy / 8.0
				case 1:
					ph = 2.0 * math.Pi * fx / 8.0
				case 2:
					ph = 2.0 * math.Pi * (fx - fy) / 8.0
				case 3:
					ph = 2.0 * math.Pi * (fx + fy) / 8.0
				}
				angBases[a][y*8+x] = math.Cos(ph)
			}
		}
	}
	pFractalAngles := labPattern{
		Code: "fractal-angles", Title: "3. Fractal 3D + Linhas (8x8 2b)",
		CellW: 8, CellH: 8, BitsPerCell: 2,
		EncodeCell: func(Y []byte, stride, x0, y0, sym, fi int, s uint64) {
			base := fractalBaseY(x0, y0, fi, s)
			b := &angBases[sym]
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					Y[row+x] = clampYUV(int32(math.Round(base+34.0*b[y*8+x])), 16, 240)
				}
			}
		},
		DecodeCell: func(Y []byte, stride, x0, y0, _ int, _ uint64) int {
			var mean float64
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					mean += float64(Y[row+x])
				}
			}
			mean /= 64.0
			bestSym := 0
			bestDot := -1e18
			for a := 0; a < 4; a++ {
				b := &angBases[a]
				var dot float64
				for y := 0; y < 8; y++ {
					row := (y0+y)*stride + x0
					for x := 0; x < 8; x++ {
						dot += (float64(Y[row+x]) - mean) * b[y*8+x]
					}
				}
				if dot > bestDot {
					bestDot = dot
					bestSym = a
				}
			}
			return bestSym
		},
	}

	// 4. Vitral Bauhaus 8x8 DCT-aligned (4 cuts \, /, -, | x 2 polarities = 3b/cell, smooth contrast)
	pBauhaus8 := makeTemplatePattern("bauhaus-8x8", "4. Vitral Bauhaus (8x8 3b)", 8, 3, func(sym, x, y int) float64 {
		cut := sym & 3
		pol := (sym >> 2) & 1
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
		return 56.0 + alpha*144.0
	})

	// 5. Curvas Truchet 8x8 DCT-aligned (2 seamless arcs x 2 tiers = 2b/cell, clean continuous maze!)
	pTruchet8 := makeTemplatePattern("truchet-8x8", "5. Curvas Truchet (8x8 2b)", 8, 2, func(sym, x, y int) float64 {
		orient := sym & 1
		tier := (sym >> 1) & 1
		fx, fy := float64(x)+0.5, float64(y)+0.5
		r := 4.0
		var dist float64
		if orient == 0 {
			dist = math.Min(math.Abs(math.Hypot(fx, fy)-r), math.Abs(math.Hypot(fx-8, fy-8)-r))
		} else {
			dist = math.Min(math.Abs(math.Hypot(fx-8, fy)-r), math.Abs(math.Hypot(fx, fy-8)-r))
		}
		rib := math.Max(0, math.Min(1, (1.6-dist)/1.1))
		if tier == 0 {
			return 44.0 + rib*118.0
		}
		return 104.0 + rib*118.0
	})

	// 6. Constelacao / Halftone Suave 8x8 (4 quadrant offsets x 2 polarities = 3b/cell, DCT-aligned!)
	pConstel8 := makeTemplatePattern("constel-8x8", "6. Constelacao Bokeh (8x8 3b)", 8, 3, func(sym, x, y int) float64 {
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
		d := math.Hypot(float64(x)+0.5-ox, float64(y)+0.5-oy)
		orb := math.Max(0, math.Min(1, (2.7-d)/1.4))
		if pol == 0 {
			return 48.0 + orb*156.0
		}
		return 204.0 - orb*156.0
	})

	patterns := []labPattern{pFractal3D, pAngles8Opt, pFractalAngles, pBauhaus8, pTruchet8, pConstel8}
	dir := t.TempDir()

	const (
		tileW = 420
		tileH = 340
		hdrH  = 34
		pad   = 14
		colsG = 3
		rowsG = 2
	)
	panelW := pad + colsG*(tileW+pad)
	panelH := pad + rowsG*(tileH+hdrH+pad)
	panel := image.NewRGBA(image.Rect(0, 0, panelW, panelH))
	for i := 0; i < len(panel.Pix); i += 4 {
		panel.Pix[i], panel.Pix[i+1], panel.Pix[i+2], panel.Pix[i+3] = 16, 18, 26, 255
	}

	t.Logf("=== ROUND 2: 8x8 DCT-ALIGNED ARTISTIC PATTERNS (2K @ 15fps | 128 KB | RS 25%%) ===")
	t.Logf("%-16s | %5s | %3s | %-18s | %-18s | %-15s | %-15s",
		"Pattern", "B/f", "Frm", "H.264 Up (CRF34)", "AV1 Up (CRF44)", "YT VP9 (fromH)", "YT VP9 (fromA)")

	for idx := range patterns {
		p := &patterns[idx]
		_, _, _, capPerFrame := p.frameCapacities(W, H)
		nFrames := (payloadLen + capPerFrame - 1) / capPerFrame
		frames := make([]*YUVFrame, nFrames)
		for fi := 0; fi < nFrames; fi++ {
			f := NewYUVFrame(W, H)
			off := fi * capPerFrame
			end := off + capPerFrame
			if end > payloadLen {
				end = payloadLen
			}
			p.encodeFrame(f, payload[off:end], fi, seed)
			frames[fi] = f
		}

		// Visual panel
		crop := yuvCropToRGBA(frames[0], 640, 360, tileW, tileH)
		gc, gr := idx%colsG, idx/colsG
		px0, py0 := pad+gc*(tileW+pad), pad+gr*(tileH+hdrH+pad)
		for y := 0; y < hdrH; y++ {
			for x := 0; x < tileW; x++ {
				panel.SetRGBA(px0+x, py0+y, color.RGBA{R: 28, G: 34, B: 48, A: 255})
			}
		}
		drawText(panel, px0+8, py0+10, 2, p.Title, color.RGBA{R: 235, G: 242, B: 255, A: 255})
		for y := 0; y < tileH; y++ {
			for x := 0; x < tileW; x++ {
				panel.SetRGBA(px0+x, py0+hdrH+y, crop.RGBAAt(x, y))
			}
		}

		h264Path := filepath.Join(dir, p.Code+"_h264.mp4")
		hSize, hDur, err := runFFmpegEncodeRaw(tools.FFmpegPath, W, H, FPS, frames,
			[]string{"-c:v", "libx264", "-preset", "medium", "-crf", "34"}, h264Path)
		if err != nil {
			t.Fatalf("%s h264: %v", p.Code, err)
		}
		hOk, hTot, hCorr, hMatch := decodeVideoAndVerify(tools.FFmpegPath, h264Path, W, H, p, payload, seed)

		av1Path := filepath.Join(dir, p.Code+"_av1.mp4")
		aSize, aDur, err := runFFmpegEncodeRaw(tools.FFmpegPath, W, H, FPS, frames,
			[]string{"-c:v", "libaom-av1", "-usage", "realtime", "-cpu-used", "8", "-crf", "44", "-b:v", "0", "-row-mt", "1", "-tiles", "2x2"}, av1Path)
		if err != nil {
			t.Fatalf("%s av1: %v", p.Code, err)
		}
		aOk, aTot, aCorr, aMatch := decodeVideoAndVerify(tools.FFmpegPath, av1Path, W, H, p, payload, seed)

		vp9HPath := filepath.Join(dir, p.Code+"_vp9h.webm")
		vp9HSize, err := runFFmpegTranscode(tools.FFmpegPath, h264Path, vp9HPath,
			[]string{"-c:v", "libvpx-vp9", "-deadline", "good", "-cpu-used", "5", "-b:v", "10M", "-row-mt", "1"})
		if err != nil {
			t.Fatalf("%s vp9h: %v", p.Code, err)
		}
		vhOk, vhTot, vhCorr, vhMatch := decodeVideoAndVerify(tools.FFmpegPath, vp9HPath, W, H, p, payload, seed)

		vp9APath := filepath.Join(dir, p.Code+"_vp9a.webm")
		vp9ASize, err := runFFmpegTranscode(tools.FFmpegPath, av1Path, vp9APath,
			[]string{"-c:v", "libvpx-vp9", "-deadline", "good", "-cpu-used", "5", "-b:v", "10M", "-row-mt", "1"})
		if err != nil {
			t.Fatalf("%s vp9a: %v", p.Code, err)
		}
		vaOk, vaTot, vaCorr, vaMatch := decodeVideoAndVerify(tools.FFmpegPath, vp9APath, W, H, p, payload, seed)

		fmtStatus := func(sz int64, ok, tot, corr int, match bool, dt time.Duration) string {
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

		t.Logf("%-16s | %5d | %3d | %-18s | %-18s | %-15s | %-15s",
			p.Code, capPerFrame, nFrames,
			fmtStatus(hSize, hOk, hTot, hCorr, hMatch, hDur),
			fmtStatus(aSize, aOk, aTot, aCorr, aMatch, aDur),
			fmtStatus(vp9HSize, vhOk, vhTot, vhCorr, vhMatch, 0),
			fmtStatus(vp9ASize, vaOk, vaTot, vaCorr, vaMatch, 0),
		)
	}

	if outPNG := os.Getenv("YTCLOUD_PATTERN_PNG"); outPNG != "" {
		f, err := os.Create(outPNG)
		if err != nil {
			t.Fatalf("create round2 PNG: %v", err)
		}
		defer f.Close()
		_ = png.Encode(f, panel)
		t.Logf("Saved Round 2 preview PNG: %s (%dx%d)", outPNG, panelW, panelH)
	}
}

// TestArtisticPatternLabRound3 calibrates Curvas Truchet 8x8, Fractal 3D + Linhas 8x8,
// Linhas & Angulos 8x8 (with diagonal frequency pre-emphasis & RS 30%), plus two brand-new
// lowest-frequency DCT(1,0)/DCT(0,1) 3D Crystal Light-Angle patterns over 3D Fractals.
func TestArtisticPatternLabRound3(t *testing.T) {
	if os.Getenv("YTCLOUD_PATTERN_LAB") != "3" {
		t.Skip("set YTCLOUD_PATTERN_LAB=3 to run round 3")
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
		seed       = uint64(0xA171571CF1AC7A1)
	)
	payload := make([]byte, payloadLen)
	rand.New(rand.NewSource(20261123)).Read(payload)

	fractalBaseY := func(x0, y0, frameIdx int, s uint64) float64 {
		s32 := uint32(s ^ (s >> 32))
		tm := float64(frameIdx) * 0.045
		nx := (float64(x0+4) / 2560.0) * 3.4 * 1.777
		ny := (float64(y0+4) / 1440.0) * 3.4
		qx := fractalFBM3(nx+tm*0.5, ny-tm*0.35, s32)
		qy := fractalFBM3(nx+5.2-tm*0.4, ny+1.3+tm*0.55, s32+101)
		r := fractalFBM3(nx+2.2*qx+1.7, ny+2.2*qy+9.2, s32+211)
		c := math.Max(0, math.Min(1, (r-0.18)*1.55))
		return 72.0 + c*112.0 // [72..184] leaves generous ±56 headroom!
	}

	// Precompute 4 orthogonal 8x8 wave bases (0, 90, 45, 135 deg) with diagonal pre-emphasis
	var angBases [4][64]float64
	var angAmps = [4]float64{46.0, 46.0, 56.0, 56.0}
	for a := 0; a < 4; a++ {
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				fx, fy := float64(x)+0.5, float64(y)+0.5
				var ph float64
				switch a {
				case 0:
					ph = 2.0 * math.Pi * fy / 8.0
				case 1:
					ph = 2.0 * math.Pi * fx / 8.0
				case 2:
					ph = 2.0 * math.Pi * (fx - fy) / 8.0
				case 3:
					ph = 2.0 * math.Pi * (fx + fy) / 8.0
				}
				angBases[a][y*8+x] = math.Cos(ph)
			}
		}
	}

	// 1. Fractal 3D + Linhas/Angulos (8x8, 2b, pre-emphasized ±46/±56, RS 30%)
	pFractalAngles50 := labPattern{
		Code: "fractal-ang-50", Title: "1. Fractal 3D + Linhas (8x8 2b)",
		CellW: 8, CellH: 8, BitsPerCell: 2,
		EncodeCell: func(Y []byte, stride, x0, y0, sym, fi int, s uint64) {
			base := fractalBaseY(x0, y0, fi, s)
			b := &angBases[sym]
			amp := angAmps[sym]
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					Y[row+x] = clampYUV(int32(math.Round(base+amp*b[y*8+x])), 16, 240)
				}
			}
		},
		DecodeCell: func(Y []byte, stride, x0, y0, _ int, _ uint64) int {
			var mean float64
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					mean += float64(Y[row+x])
				}
			}
			mean /= 64.0
			bestSym, bestDot := 0, -1e18
			for a := 0; a < 4; a++ {
				b := &angBases[a]
				var dot float64
				for y := 0; y < 8; y++ {
					row := (y0+y)*stride + x0
					for x := 0; x < 8; x++ {
						dot += (float64(Y[row+x]) - mean) * b[y*8+x]
					}
				}
				// Slight boost to diagonal correlation to compensate VP9 high-freq attenuation
				if a >= 2 {
					dot *= 1.12
				}
				if dot > bestDot {
					bestDot, bestSym = dot, a
				}
			}
			return bestSym
		},
	}

	// 2. Linhas & Angulos 8x8 Calibrado (4 pre-emphasized angles x 2 DC levels = 3b/cell, RS 30%)
	pAnglesSweet := makeTemplatePattern("angles-8-sweet", "2. Linhas & Angulos Calib (8x8 3b)", 8, 3, func(sym, x, y int) float64 {
		a := sym & 3
		dc := 84.0
		if (sym>>2)&1 == 1 {
			dc = 172.0
		}
		return dc + angAmps[a]*angBases[a][y*8+x]
	})

	// 3. Curvas Truchet 8x8 Calibrado (2 seamless arcs x 2 high-contrast tiers = 2b/cell, RS 30%)
	pTruchetV2 := makeTemplatePattern("truchet-8-v2", "3. Curvas Truchet Calib (8x8 2b)", 8, 2, func(sym, x, y int) float64 {
		orient := sym & 1
		tier := (sym >> 1) & 1
		fx, fy := float64(x)+0.5, float64(y)+0.5
		r := 4.0
		var dist float64
		if orient == 0 {
			dist = math.Min(math.Abs(math.Hypot(fx, fy)-r), math.Abs(math.Hypot(fx-8, fy-8)-r))
		} else {
			dist = math.Min(math.Abs(math.Hypot(fx-8, fy)-r), math.Abs(math.Hypot(fx, fy-8)-r))
		}
		// Slightly fuller ribbon (width ~2.1px) so VP9 never smears the curves
		rib := math.Max(0, math.Min(1, (1.85-dist)/1.05))
		if tier == 0 {
			return 24.0 + rib*152.0 // dark obsidian + crisp glowing arcs
		}
		return 104.0 + rib*134.0 // bright luminous mist + pure white arcs
	})

	// 4. Fractal 3D + Curvas Truchet (8x8, 2b = 2 arc orientations x 2 polarities etched over 3D Fractal!)
	var truchZero [2][64]float64
	for o := 0; o < 2; o++ {
		var sum float64
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				fx, fy := float64(x)+0.5, float64(y)+0.5
				r := 4.0
				var dist float64
				if o == 0 {
					dist = math.Min(math.Abs(math.Hypot(fx, fy)-r), math.Abs(math.Hypot(fx-8, fy-8)-r))
				} else {
					dist = math.Min(math.Abs(math.Hypot(fx-8, fy)-r), math.Abs(math.Hypot(fx, fy-8)-r))
				}
				rib := math.Max(0, math.Min(1, (1.85-dist)/1.05))
				truchZero[o][y*8+x] = rib
				sum += rib
			}
		}
		m := sum / 64.0
		var maxAbs float64
		for i := 0; i < 64; i++ {
			truchZero[o][i] -= m
			if math.Abs(truchZero[o][i]) > maxAbs {
				maxAbs = math.Abs(truchZero[o][i])
			}
		}
		for i := 0; i < 64; i++ {
			truchZero[o][i] /= maxAbs
		}
	}
	pFractalTruchet := labPattern{
		Code: "fractal-truch", Title: "4. Fractal 3D + Truchet (8x8 2b)",
		CellW: 8, CellH: 8, BitsPerCell: 2,
		EncodeCell: func(Y []byte, stride, x0, y0, sym, fi int, s uint64) {
			base := fractalBaseY(x0, y0, fi, s)
			o := sym & 1
			sign := 1.0
			if (sym>>1)&1 == 1 {
				sign = -1.0
			}
			t := &truchZero[o]
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					Y[row+x] = clampYUV(int32(math.Round(base+sign*54.0*t[y*8+x])), 16, 240)
				}
			}
		},
		DecodeCell: func(Y []byte, stride, x0, y0, _ int, _ uint64) int {
			var mean float64
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					mean += float64(Y[row+x])
				}
			}
			mean /= 64.0
			bestSym, bestDot := 0, -1e18
			for s := 0; s < 4; s++ {
				o := s & 1
				sign := 1.0
				if (s>>1)&1 == 1 {
					sign = -1.0
				}
				t := &truchZero[o]
				var dot float64
				for y := 0; y < 8; y++ {
					row := (y0+y)*stride + x0
					for x := 0; x < 8; x++ {
						dot += (float64(Y[row+x]) - mean) * sign * t[y*8+x]
					}
				}
				if dot > bestDot {
					bestDot, bestSym = dot, s
				}
			}
			return bestSym
		},
	}

	// 5. Cristal 3D — 8 Angulos de Luz DCT(1,0)/DCT(0,1) sobre Fractal 3D (8x8, 3b, RS 25%!)
	// Uses ONLY the 2 lowest-frequency AC coefficients in H.264/VP9/AV1!
	var dctX, dctY [64]float64
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			dctX[y*8+x] = math.Cos(math.Pi * (float64(x) + 0.5) / 8.0)
			dctY[y*8+x] = math.Cos(math.Pi * (float64(y) + 0.5) / 8.0)
		}
	}
	gray8Enc := [8]int{0, 1, 3, 2, 6, 7, 5, 4}
	gray8Dec := [8]int{0, 1, 3, 2, 7, 6, 4, 5}
	pFacets8 := labPattern{
		Code: "facets-8dir", Title: "5. Cristal 3D 8-Ang DCT1 (8x8 3b)",
		CellW: 8, CellH: 8, BitsPerCell: 3,
		EncodeCell: func(Y []byte, stride, x0, y0, sym, fi int, s uint64) {
			base := fractalBaseY(x0, y0, fi, s)
			sector := gray8Enc[sym]
			theta := float64(sector) * math.Pi / 4.0 // 0, 45, 90, ..., 315 deg
			cx, cy := math.Cos(theta)*54.0, math.Sin(theta)*54.0
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					i := y*8 + x
					Y[row+x] = clampYUV(int32(math.Round(base+cx*dctX[i]+cy*dctY[i])), 16, 240)
				}
			}
		},
		DecodeCell: func(Y []byte, stride, x0, y0, _ int, _ uint64) int {
			var gx, gy float64
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					i := y*8 + x
					v := float64(Y[row+x])
					gx += v * dctX[i]
					gy += v * dctY[i]
				}
			}
			ang := math.Atan2(gy, gx)
			if ang < 0 {
				ang += 2.0 * math.Pi
			}
			sector := int(math.Floor((ang+math.Pi/8.0)/(math.Pi/4.0))) & 7
			return gray8Dec[sector]
		},
	}

	// 6. Cristal 3D 4-Angulos + 2 Tons DC (8x8, 3b — 90-degree angular separation + DCT(1,0)/DCT(0,1)!)
	pFacets4DC := labPattern{
		Code: "facets-4dc", Title: "6. Cristal 4-Ang + DC (8x8 3b)",
		CellW: 8, CellH: 8, BitsPerCell: 3,
		EncodeCell: func(Y []byte, stride, x0, y0, sym, _ int, _ uint64) {
			sector := sym & 3 // 0, 90, 180, 270 deg
			dc := 84.0
			if (sym>>2)&1 == 1 {
				dc = 172.0
			}
			theta := float64(sector) * math.Pi / 2.0
			cx, cy := math.Cos(theta)*46.0, math.Sin(theta)*46.0
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					i := y*8 + x
					Y[row+x] = clampYUV(int32(math.Round(dc+cx*dctX[i]+cy*dctY[i])), 16, 240)
				}
			}
		},
		DecodeCell: func(Y []byte, stride, x0, y0, _ int, _ uint64) int {
			var mean, gx, gy float64
			for y := 0; y < 8; y++ {
				row := (y0+y)*stride + x0
				for x := 0; x < 8; x++ {
					i := y*8 + x
					v := float64(Y[row+x])
					mean += v
					gx += v * dctX[i]
					gy += v * dctY[i]
				}
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
		},
	}

	type r3Case struct {
		p       labPattern
		rsData  int // 179 (30% FEC) or 191 (25% FEC)
		h264CRF string
		av1CRF  string
	}
	cases := []r3Case{
		{pFractalAngles50, 179, "30", "40"},
		{pAnglesSweet, 179, "30", "40"},
		{pTruchetV2, 179, "30", "40"},
		{pFractalTruchet, 179, "30", "40"},
		{pFacets8, 191, "32", "42"},
		{pFacets4DC, 191, "34", "44"},
	}

	dir := t.TempDir()
	const (
		tileW = 420
		tileH = 340
		hdrH  = 34
		pad   = 14
		colsG = 3
		rowsG = 2
	)
	panelW := pad + colsG*(tileW+pad)
	panelH := pad + rowsG*(tileH+hdrH+pad)
	panel := image.NewRGBA(image.Rect(0, 0, panelW, panelH))
	for i := 0; i < len(panel.Pix); i += 4 {
		panel.Pix[i], panel.Pix[i+1], panel.Pix[i+2], panel.Pix[i+3] = 16, 18, 26, 255
	}

	// Custom RS-parameterized helpers so we can benchmark RS 30% (179) vs RS 25% (191) cleanly
	encodeFrameRS := func(p *labPattern, dst *YUVFrame, chunk []byte, fi int, rsData int) {
		cols, rows, rawBytes, _ := p.frameCapacities(dst.Width, dst.Height)
		rs := GetRSCodec(rsData)
		raw := rs.EncodeInterleaved(chunk, (rawBytes/255)*255)
		if len(raw) < rawBytes {
			padBuf := make([]byte, rawBytes)
			copy(padBuf, raw)
			raw = padBuf
		}
		U, V := RenderFractalChromaFields(dst.Width/2, dst.Height/2, fi, seed, 48, 208)
		copy(dst.UPlane(), U)
		copy(dst.VPlane(), V)
		Y := dst.YPlane()
		for i := range Y {
			Y[i] = 28
		}
		const margin = 16
		bitPos := 0
		readBits := func(n int) int {
			v := 0
			for i := 0; i < n; i++ {
				bIdx := bitPos >> 3
				bShift := uint(7 - (bitPos & 7))
				bit := 0
				if bIdx < len(raw) {
					bit = int((raw[bIdx] >> bShift) & 1)
				}
				v = (v << 1) | bit
				bitPos++
			}
			return v
		}
		for r := 0; r < rows; r++ {
			y0 := margin + r*p.CellH
			for c := 0; c < cols; c++ {
				p.EncodeCell(Y, dst.Width, margin+c*p.CellW, y0, readBits(p.BitsPerCell), fi, seed)
			}
		}
	}

	decodeVideoRS := func(vidPath string, p *labPattern, rsData int) (okFrames, totalFrames, totalCorr int, match bool) {
		cols, rows, rawBytes, _ := p.frameCapacities(W, H)
		capPerFrame := (rawBytes / 255) * rsData
		totalFrames = (payloadLen + capPerFrame - 1) / capPerFrame
		cmd := exec.Command(tools.FFmpegPath, "-loglevel", "error", "-i", vidPath, "-f", "rawvideo", "-pix_fmt", "yuv420p", "pipe:1")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return 0, totalFrames, 0, false
		}
		if err := cmd.Start(); err != nil {
			return 0, totalFrames, 0, false
		}
		buf := make([]byte, W*H*3/2)
		recovered := make([]byte, 0, payloadLen)
		rs := GetRSCodec(rsData)
		const margin = 16
		for fi := 0; fi < totalFrames; fi++ {
			if _, err := io.ReadFull(stdout, buf); err != nil {
				break
			}
			Y := buf[:W*H]
			raw := make([]byte, rawBytes)
			bitPos := 0
			for r := 0; r < rows; r++ {
				y0 := margin + r*p.CellH
				for c := 0; c < cols; c++ {
					sym := p.DecodeCell(Y, W, margin+c*p.CellW, y0, fi, seed)
					for i := p.BitsPerCell - 1; i >= 0; i-- {
						bIdx := bitPos >> 3
						bShift := uint(7 - (bitPos & 7))
						if bIdx < len(raw) && ((sym>>uint(i))&1) != 0 {
							raw[bIdx] |= 1 << bShift
						}
						bitPos++
					}
				}
			}
			chunk, corr, err := rs.DecodeInterleaved(raw[:(rawBytes/255)*255])
			if err != nil {
				continue
			}
			okFrames++
			totalCorr += corr
			off := fi * capPerFrame
			need := payloadLen - off
			if need > capPerFrame {
				need = capPerFrame
			}
			recovered = append(recovered, chunk[:need]...)
		}
		_ = cmd.Wait()
		return okFrames, totalFrames, totalCorr, okFrames == totalFrames && len(recovered) == payloadLen && bytes.Equal(recovered, payload)
	}

	t.Logf("=== ROUND 3: CALIBRATED TRUCHET + FRACTAL-ANGLES + 3D CRYSTAL DCT(1,0/0,1) (2K @ 15fps | 128 KB) ===")
	t.Logf("%-15s | %3s | %3s | %-18s | %-18s | %-15s | %-15s | %-15s",
		"Pattern", "FEC", "Frm", "H.264 Up", "AV1 Up", "YT VP9 (fromH)", "YT VP9 (fromA)", "YT H264 (best)")

	for idx := range cases {
		tc := &cases[idx]
		p := &tc.p
		_, _, rawBytes, _ := p.frameCapacities(W, H)
		capPerFrame := (rawBytes / 255) * tc.rsData
		nFrames := (payloadLen + capPerFrame - 1) / capPerFrame
		frames := make([]*YUVFrame, nFrames)
		for fi := 0; fi < nFrames; fi++ {
			f := NewYUVFrame(W, H)
			off := fi * capPerFrame
			end := off + capPerFrame
			if end > payloadLen {
				end = payloadLen
			}
			encodeFrameRS(p, f, payload[off:end], fi, tc.rsData)
			frames[fi] = f
		}

		crop := yuvCropToRGBA(frames[0], 640, 360, tileW, tileH)
		gc, gr := idx%colsG, idx/colsG
		px0, py0 := pad+gc*(tileW+pad), pad+gr*(tileH+hdrH+pad)
		for y := 0; y < hdrH; y++ {
			for x := 0; x < tileW; x++ {
				panel.SetRGBA(px0+x, py0+y, color.RGBA{R: 28, G: 34, B: 48, A: 255})
			}
		}
		drawText(panel, px0+8, py0+10, 2, p.Title, color.RGBA{R: 235, G: 242, B: 255, A: 255})
		for y := 0; y < tileH; y++ {
			for x := 0; x < tileW; x++ {
				panel.SetRGBA(px0+x, py0+hdrH+y, crop.RGBAAt(x, y))
			}
		}

		h264Path := filepath.Join(dir, p.Code+"_h264.mp4")
		hSize, hDur, err := runFFmpegEncodeRaw(tools.FFmpegPath, W, H, FPS, frames,
			[]string{"-c:v", "libx264", "-preset", "medium", "-crf", tc.h264CRF}, h264Path)
		if err != nil {
			t.Fatalf("%s h264: %v", p.Code, err)
		}
		hOk, hTot, hCorr, hMatch := decodeVideoRS(h264Path, p, tc.rsData)

		av1Path := filepath.Join(dir, p.Code+"_av1.mp4")
		aSize, aDur, err := runFFmpegEncodeRaw(tools.FFmpegPath, W, H, FPS, frames,
			[]string{"-c:v", "libaom-av1", "-usage", "realtime", "-cpu-used", "8", "-crf", tc.av1CRF, "-b:v", "0", "-row-mt", "1", "-tiles", "2x2"}, av1Path)
		if err != nil {
			t.Fatalf("%s av1: %v", p.Code, err)
		}
		aOk, aTot, aCorr, aMatch := decodeVideoRS(av1Path, p, tc.rsData)

		vp9HPath := filepath.Join(dir, p.Code+"_vp9h.webm")
		vp9HSize, _ := runFFmpegTranscode(tools.FFmpegPath, h264Path, vp9HPath,
			[]string{"-c:v", "libvpx-vp9", "-deadline", "good", "-cpu-used", "5", "-b:v", "10M", "-row-mt", "1"})
		vhOk, vhTot, vhCorr, vhMatch := decodeVideoRS(vp9HPath, p, tc.rsData)

		vp9APath := filepath.Join(dir, p.Code+"_vp9a.webm")
		vp9ASize, _ := runFFmpegTranscode(tools.FFmpegPath, av1Path, vp9APath,
			[]string{"-c:v", "libvpx-vp9", "-deadline", "good", "-cpu-used", "5", "-b:v", "10M", "-row-mt", "1"})
		vaOk, vaTot, vaCorr, vaMatch := decodeVideoRS(vp9APath, p, tc.rsData)

		// Also test YT H.264 10M from whichever master was smaller!
		bestMaster := h264Path
		if aMatch && aSize < hSize {
			bestMaster = av1Path
		}
		ythPath := filepath.Join(dir, p.Code+"_yth264.mp4")
		ythSize, _ := runFFmpegTranscode(tools.FFmpegPath, bestMaster, ythPath,
			[]string{"-c:v", "libx264", "-preset", "medium", "-b:v", "10M", "-maxrate", "12M", "-bufsize", "20M"})
		yhOk, yhTot, yhCorr, yhMatch := decodeVideoRS(ythPath, p, tc.rsData)

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

		fecPct := fmt.Sprintf("%d%%", (255-tc.rsData)*100/255)
		t.Logf("%-15s | %3s | %3d | %-18s | %-18s | %-15s | %-15s | %-15s",
			p.Code, fecPct, nFrames,
			fmtS(hSize, hOk, hTot, hCorr, hMatch, hDur),
			fmtS(aSize, aOk, aTot, aCorr, aMatch, aDur),
			fmtS(vp9HSize, vhOk, vhTot, vhCorr, vhMatch, 0),
			fmtS(vp9ASize, vaOk, vaTot, vaCorr, vaMatch, 0),
			fmtS(ythSize, yhOk, yhTot, yhCorr, yhMatch, 0),
		)
	}

	if outPNG := os.Getenv("YTCLOUD_PATTERN_PNG"); outPNG != "" {
		f, err := os.Create(outPNG)
		if err != nil {
			t.Fatalf("create round3 PNG: %v", err)
		}
		defer f.Close()
		_ = png.Encode(f, panel)
		t.Logf("Saved Round 3 preview PNG: %s (%dx%d)", outPNG, panelW, panelH)
	}
}

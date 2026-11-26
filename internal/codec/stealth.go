package codec

import (
	"errors"
	"fmt"
	"math"
)

// ============================================================================
// STEALTH OPTICAL MASKS (Zero-Grid Blind-Decodable Aesthetic Carriers)
//   1. stealth-vhs   : 32x8 horizontal DCT(0,1) analog scanlines + chroma bleed
//   2. stealth-rain  : 22x26 diagonal falling raindrop PPM streaks (2 bits/drop)
//   3. stealth-film  : 16x16 Hanning-windowed 4x4-clump 35mm silver-halide grain
//   4. stealth-stars : 32x30 full-screen starlight constellation mask (3 bits/star)
//
// When FlagHiddenHeader is set, ALL 4 masks cover 100% of the frame edge-to-edge
// with ZERO header bar, ZERO square grid, and ZERO borders — embedding the 3x32B
// CRC32 FrameHeader natively inside the first 96 bytes of the stealth mask itself!
// ============================================================================

const (
	stealthHeaderBytes = HeaderCopies * FrameHeaderSize // 3 * 32 = 96 bytes

	stealthVHSCellW  = 32
	stealthVHSCellH  = 8
	stealthRainCellW = 22
	stealthRainCellH = 26
	stealthRainLen   = 18
	stealthFilmCell  = 16
	stealthStarCellW = 32
	stealthStarCellH = 30
)

var (
	stealthFilmWin [stealthFilmCell * stealthFilmCell]float64
	stealthFilmIdx [stealthFilmCell]int
	stealthFilmW0  [stealthFilmCell]float64
	stealthFilmW1  [stealthFilmCell]float64
)

func init() {
	const g = stealthFilmCell / 4
	for i := 0; i < stealthFilmCell; i++ {
		f := (float64(i) + 0.5) * float64(g-1) / float64(stealthFilmCell)
		idx := int(f)
		if idx >= g-1 {
			idx = g - 2
		}
		w1 := f - float64(idx)
		stealthFilmIdx[i] = idx
		stealthFilmW0[i] = 1.0 - w1
		stealthFilmW1[i] = w1
	}
	for y := 0; y < stealthFilmCell; y++ {
		wy := math.Sin(math.Pi * (float64(y) + 0.5) / float64(stealthFilmCell))
		for x := 0; x < stealthFilmCell; x++ {
			wx := math.Sin(math.Pi * (float64(x) + 0.5) / float64(stealthFilmCell))
			stealthFilmWin[y*stealthFilmCell+x] = wx * wy
		}
	}
}

// IsStealthArtMode reports whether mode is one of the 4 zero-grid stealth optical masks.
func IsStealthArtMode(mode string) bool {
	switch mode {
	case "stealth-vhs", "stealth-rain", "stealth-film", "stealth-stars":
		return true
	default:
		return false
	}
}

func stealthMixHash(a, b, c uint64) uint64 {
	x := a*0x9E3779B97F4A7C15 ^ b*0xBF58476D1CE4E5B9 ^ c*0x94D049BB133111EB
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	return x ^ (x >> 31)
}

func stealthClampY(v float64) byte {
	if v < 16 {
		return 16
	}
	if v > 235 {
		return 235
	}
	return byte(math.Round(v))
}

// stealthGridBounds returns the active pixel rect (x0, y0, dw, dh) and header-byte prefix count.
func stealthGridBounds(w, h int, layoutV2 bool, flags uint8) (x0, y0, dw, dh, hdrBytes int) {
	if layoutV2 && flags&FlagHiddenHeader != 0 {
		return 0, 0, w, h, stealthHeaderBytes
	}
	mx, topY, botPad := FrameLayoutMarginsForProfile(w, layoutV2, flags, false)
	return mx, topY, w - 2*mx, h - topY - botPad, 0
}

// stealthTotalBytes computes total raw bytes across the grid rect before subtracting any native header.
func stealthTotalBytes(mode string, dw, dh int) int {
	if dw <= 0 || dh <= 0 {
		return 0
	}
	switch mode {
	case "stealth-vhs":
		return ((dw / stealthVHSCellW) * (dh / stealthVHSCellH)) / 8
	case "stealth-rain":
		return ((dw / stealthRainCellW) * (dh / stealthRainCellH) * 2) / 8
	case "stealth-film":
		return ((dw / stealthFilmCell) * (dh / stealthFilmCell)) / 8
	case "stealth-stars":
		return ((dw / stealthStarCellW) * (dh / stealthStarCellH) * 3) / 8
	default:
		return 0
	}
}

// stealthRawPayloadCapacity returns raw RS-available payload bytes for a stealth mode.
func stealthRawPayloadCapacity(mode string, w, h int, layoutV2 bool, flags uint8) int {
	_, _, dw, dh, hdrBytes := stealthGridBounds(w, h, layoutV2, flags)
	tot := stealthTotalBytes(mode, dw, dh) - hdrBytes
	if tot < 0 {
		return 0
	}
	return tot
}

// renderStealthFallbackCanvas paints a clean deep twilight gradient when neither host video nor art effect is chosen.
func renderStealthFallbackCanvas(dst *YUVFrame, frameIdx int) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW := w / 2
	t := float64(frameIdx) * 0.04
	for y := 0; y < h; y++ {
		fy := float64(y) / float64(h)
		row := Y[y*w : (y+1)*w]
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(w)
			wave := 12.0 * math.Sin(fx*5.0+t) * math.Cos(fy*3.5-t*0.7)
			row[x] = stealthClampY(34.0 + 36.0*(1.0-fy) + wave)
		}
	}
	for cy := 0; cy < h/2; cy++ {
		fy := float64(cy) / float64(h/2)
		off := cy * uvW
		for cx := 0; cx < uvW; cx++ {
			U[off+cx] = stealthClampY(144.0 + 14.0*(1.0-fy))
			V[off+cx] = stealthClampY(122.0 + 8.0*fy)
		}
	}
}

func stealthHeaderRSWhitener(i int) byte {
	return byte(stealthMixHash(uint64(i), 0x59544332, 0x52533936))
}

// encodeStealthHeaderRS encodes the 32-byte FrameHeader into a 96-byte shortened RS(96, 32)
// codeword (correcting up to 32 completely corrupted bytes = 33.3% BER!), XOR-whitened.
func encodeStealthHeaderRS(hdr FrameHeader) [stealthHeaderBytes]byte {
	hb := hdr.Marshal()
	rs := GetRSCodec(191) // 64 parity bytes -> corrects up to 32 arbitrary byte errors!
	var msg [191]byte
	copy(msg[159:], hb[:])
	var cw [255]byte
	rs.EncodeBlock(msg[:], cw[:])
	var out [stealthHeaderBytes]byte
	for i := 0; i < stealthHeaderBytes; i++ {
		out[i] = cw[159+i] ^ stealthHeaderRSWhitener(i)
	}
	return out
}


// decodeStealthHeaderRS recovers a 32-byte FrameHeader from 96 strided stealth bytes (up to 32 corrupted bytes!).
func decodeStealthHeaderRS(raw96 []byte) (FrameHeader, error) {
	if len(raw96) < stealthHeaderBytes {
		return FrameHeader{}, errNotStealthHeader
	}
	rs := GetRSCodec(191)
	var cw [255]byte
	for i := 0; i < stealthHeaderBytes; i++ {
		cw[159+i] = raw96[i] ^ stealthHeaderRSWhitener(i)
	}
	if _, err := rs.DecodeBlock(cw[:]); err != nil {
		return FrameHeader{}, err
	}
	for i := 0; i < 159; i++ {
		if cw[i] != 0 {
			return FrameHeader{}, errNotStealthHeader
		}
	}
	return UnmarshalFrameHeader(cw[159 : 159+FrameHeaderSize])
}

// stealthHeaderBytePos maps header byte index i (0..95) uniformly across the full frame [0..totalBytes-1],
// ensuring high-contrast logos/borders in any region can never wipe out the header!
func stealthHeaderBytePos(i, totalBytes int) int {
	return (i * totalBytes) / stealthHeaderBytes
}

// buildStealthStream weaves the 96-byte RS(96, 32) header evenly across totalBytes when hideHeader is true,
// filling the remaining (totalBytes - 96) slots sequentially with the Reed-Solomon payload.
func buildStealthStream(hdr FrameHeader, raw []byte, totalBytes int, hideHeader, isVisualFiller bool) []byte {
	if !hideHeader {
		return raw
	}
	if totalBytes < stealthHeaderBytes+len(raw) {
		totalBytes = stealthHeaderBytes + len(raw)
	}
	stream := make([]byte, totalBytes)
	isHdrSlot := make([]bool, totalBytes)

	var hdrBytes [stealthHeaderBytes]byte
	if isVisualFiller {
		// Deterministic non-RS filler bytes so DecodeFrame cleanly skips visual filler frames!
		st := hdr.ContainerID ^ (uint64(hdr.FrameIndex+1) * 0x9E3779B97F4A7C15) ^ 0x55AA33CC
		if st == 0 {
			st = 0x123456789ABCDEF
		}
		for i := 0; i < stealthHeaderBytes; i++ {
			st ^= st << 13
			st ^= st >> 7
			st ^= st << 17
			hdrBytes[i] = byte(st)
		}
	} else {
		hdrBytes = encodeStealthHeaderRS(hdr)
	}
	for i := 0; i < stealthHeaderBytes; i++ {
		pos := stealthHeaderBytePos(i, totalBytes)
		stream[pos] = hdrBytes[i]
		isHdrSlot[pos] = true
	}
	pIdx := 0
	st := hdr.ContainerID ^ (uint64(hdr.FrameIndex+1) * 0x9E3779B97F4A7C15) ^ 0xD1B54A32D192ED03
	if st == 0 {
		st = 0xCAFEBABE12345678
	}
	for pos := 0; pos < totalBytes; pos++ {
		if isHdrSlot[pos] {
			continue
		}
		if pIdx < len(raw) {
			stream[pos] = raw[pIdx]
			pIdx++
		} else {
			st ^= st << 13
			st ^= st >> 7
			st ^= st << 17
			stream[pos] = byte(st)
		}
	}
	return stream
}

// extractStealthPayload strips the 96 strided header slots out of full frame bytes, returning rawLen payload bytes.
func extractStealthPayload(full []byte, totalBytes, rawLen int, hideHeader bool) []byte {
	if !hideHeader {
		if len(full) >= rawLen {
			return full[:rawLen]
		}
		out := make([]byte, rawLen)
		copy(out, full)
		return out
	}
	isHdrSlot := make([]bool, totalBytes)
	for i := 0; i < stealthHeaderBytes; i++ {
		pos := stealthHeaderBytePos(i, totalBytes)
		if pos < totalBytes {
			isHdrSlot[pos] = true
		}
	}
	out := make([]byte, rawLen)
	pIdx := 0
	for pos := 0; pos < len(full) && pos < totalBytes && pIdx < rawLen; pos++ {
		if isHdrSlot[pos] {
			continue
		}
		out[pIdx] = full[pos]
		pIdx++
	}
	return out
}


// encodeStealthMode renders the chosen stealth optical mask directly over dst (which already holds host/art canvas).
func encodeStealthMode(dst *YUVFrame, mode string, hdr FrameHeader, raw []byte, maskIntensity uint8, hideHeader, isVisualFiller bool) {
	x0, y0, dw, dh, _ := stealthGridBounds(dst.Width, dst.Height, true, hdr.Flags)
	if isVisualFiller && hideHeader {
		x0, y0, dw, dh = 0, 0, dst.Width, dst.Height
	}
	totalBytes := stealthTotalBytes(mode, dw, dh)
	stream := buildStealthStream(hdr, raw, totalBytes, hideHeader, isVisualFiller)
	phase := int(hdr.FrameIndex % 8)

	switch mode {
	case "stealth-vhs":
		encodeStealthVHSGrid(dst, x0, y0, dw, dh, int(hdr.FrameIndex), hdr.ContainerID, stream, maskIntensity, hideHeader)
	case "stealth-rain":
		encodeStealthRainGrid(dst, x0, y0, dw, dh, phase, hdr.ContainerID, stream, maskIntensity, hideHeader)
	case "stealth-film":
		encodeStealthFilmGrid(dst, x0, y0, dw, dh, phase, hdr.ContainerID, stream, maskIntensity, hideHeader)
	case "stealth-stars":
		encodeStealthStarsGrid(dst, x0, y0, dw, dh, int(hdr.FrameIndex), hdr.ContainerID, stream, maskIntensity, hideHeader)
	}
}

func extractStealthPayloadWithConf(full []byte, fullConf []float32, totalBytes, rawLen int, hideHeader bool) ([]byte, []float32) {
	if !hideHeader {
		out := make([]byte, rawLen)
		copy(out, full)
		if len(fullConf) == 0 {
			return out, nil
		}
		outConf := make([]float32, rawLen)
		copy(outConf, fullConf)
		return out, outConf
	}
	isHdrSlot := make([]bool, totalBytes)
	for i := 0; i < stealthHeaderBytes; i++ {
		pos := stealthHeaderBytePos(i, totalBytes)
		if pos < totalBytes {
			isHdrSlot[pos] = true
		}
	}
	out := make([]byte, rawLen)
	var outConf []float32
	if len(fullConf) > 0 {
		outConf = make([]float32, rawLen)
	}
	pIdx := 0
	for pos := 0; pos < len(full) && pos < totalBytes && pIdx < rawLen; pos++ {
		if isHdrSlot[pos] {
			continue
		}
		out[pIdx] = full[pos]
		if outConf != nil && pos < len(fullConf) {
			outConf[pIdx] = fullConf[pos]
		}
		pIdx++
	}
	return out, outConf
}

// decodeStealthMode extracts raw RS payload bytes (and optional per-byte soft reliabilities) from a stealth frame.
func decodeStealthMode(src *YUVFrame, mode string, hdr FrameHeader, layoutV2 bool, rawLen int) ([]byte, []float32) {
	x0, y0, dw, dh, hdrBytes := stealthGridBounds(src.Width, src.Height, layoutV2, hdr.Flags)
	totalBytes := stealthTotalBytes(mode, dw, dh)
	hideHeader := hdrBytes > 0
	need := totalBytes
	if !hideHeader {
		need = rawLen
	}
	phase := int(hdr.FrameIndex % 8)

	var full []byte
	var fullConf []float32
	switch mode {
	case "stealth-vhs":
		full = decodeStealthVHSGrid(src, x0, y0, dw, dh, need)
	case "stealth-rain":
		full = decodeStealthRainGrid(src, x0, y0, dw, dh, phase, need)
	case "stealth-film":
		full, fullConf = decodeStealthFilmGridSoft(src, x0, y0, dw, dh, phase, need)
	case "stealth-stars":
		full = decodeStealthStarsGrid(src, x0, y0, dw, dh, need)
	default:
		return make([]byte, rawLen), nil
	}
	return extractStealthPayloadWithConf(full, fullConf, totalBytes, rawLen, hideHeader)
}

// readHeaderHiddenStealthHint fast-checks a single known stealth profile & expected phase (~0.05ms!).
func (f *YUVFrame) readHeaderHiddenStealthHint(pid ProfileID, expectedPhase int) (FrameHeader, error) {
	p, ok := Profiles[pid]
	if !ok || !IsStealthArtMode(p.ArtMode) {
		return FrameHeader{}, errNotStealthHeader
	}
	w, h := f.Width, f.Height
	phase := ((expectedPhase % 8) + 8) % 8
	var raw96 []byte
	switch p.ArtMode {
	case "stealth-vhs":
		raw96 = decodeStealthVHSHeaderSlots(f, w, h)
	case "stealth-stars":
		raw96 = decodeStealthStarsHeaderSlots(f, w, h)
	case "stealth-rain":
		raw96 = decodeStealthRainHeaderSlots(f, w, h, phase)
	case "stealth-film":
		raw96 = decodeStealthFilmHeaderSlots(f, w, h, phase)
	default:
		return FrameHeader{}, errNotStealthHeader
	}
	hdr, err := decodeStealthHeaderRS(raw96)
	if err != nil || hdr.ProfileID != pid {
		return FrameHeader{}, errNotStealthHeader
	}
	if (p.ArtMode == "stealth-rain" || p.ArtMode == "stealth-film") && int(hdr.FrameIndex%8) != phase {
		return FrameHeader{}, errNotStealthHeader
	}
	return hdr, nil
}

// readHeaderHiddenStealth attempts to detect a native full-screen Stealth FrameHeader (zero bars!).
// Decodes ONLY the 96 strided RS(96,32) header bytes across the full frame (<0.3ms even in 4K!).
func (f *YUVFrame) readHeaderHiddenStealth() (FrameHeader, error) {
	tryRS := func(raw96 []byte, expectedMode string) (FrameHeader, bool) {
		hdr, err := decodeStealthHeaderRS(raw96)
		if err != nil {
			return FrameHeader{}, false
		}
		p, ok := Profiles[hdr.ProfileID]
		if !ok || p.ArtMode != expectedMode {
			return FrameHeader{}, false
		}
		return hdr, true
	}

	w, h := f.Width, f.Height
	// 1. VHS Scanlines (phase-independent, ~0.08ms)
	if hdr, ok := tryRS(decodeStealthVHSHeaderSlots(f, w, h), "stealth-vhs"); ok {
		return hdr, nil
	}
	// 2. Starry Mask (phase-independent, ~0.12ms)
	if hdr, ok := tryRS(decodeStealthStarsHeaderSlots(f, w, h), "stealth-stars"); ok {
		return hdr, nil
	}
	// 3. Falling Rain (8 falling phases on 96 strided bytes only, ~0.25ms)
	for phase := 0; phase < 8; phase++ {
		if hdr, ok := tryRS(decodeStealthRainHeaderSlots(f, w, h, phase), "stealth-rain"); ok {
			if int(hdr.FrameIndex%8) == phase {
				return hdr, nil
			}
		}
	}
	// 4. 35mm Film Grain (8 grain phases on 96 strided bytes only, ~0.35ms)
	for phase := 0; phase < 8; phase++ {
		if hdr, ok := tryRS(decodeStealthFilmHeaderSlots(f, w, h, phase), "stealth-film"); ok {
			if int(hdr.FrameIndex%8) == phase {
				return hdr, nil
			}
		}
	}
	return FrameHeader{}, errNotStealthHeader
}

var errNotStealthHeader = errString("cabeçalho stealth não detectado")

type errString string

func (e errString) Error() string { return string(e) }

func preMapLumaHeadroom(Y []byte, w, x0, y0, dw, dh, lo, hi int) {
	span := hi - lo
	for y := y0; y < y0+dh; y++ {
		row := Y[y*w+x0 : y*w+x0+dw]
		for x := range row {
			v := lo + ((int(row[x])-16)*span)/219
			if v < lo {
				v = lo
			} else if v > hi {
				v = hi
			}
			row[x] = byte(v)
		}
	}
}

// ----------------------------------------------------------------------------
// 1. STEALTH VHS ANALOG SCANLINES (32x8 horizontal DCT(0,1) pairs + chroma bleed)
// ----------------------------------------------------------------------------
func encodeStealthVHSGrid(dst *YUVFrame, x0, y0, dw, dh, frameIdx int, seed uint64, stream []byte, maskIntensity uint8, fullChroma bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	preMapLumaHeadroom(Y, w, x0, y0, dw, dh, 28, 222)
	uvW, uvH := w/2, h/2

	// Authentic VHS chroma shift (U +2px right, V -2px left) + subtle warm analog tracking wave
	origU := append([]byte(nil), U...)
	origV := append([]byte(nil), V...)
	cyStart := 0
	if !fullChroma {
		cyStart = y0 / 2
	}
	for cy := cyStart; cy < uvH; cy++ {
		off := cy * uvW
		wave := int32(math.Round(3.0 * math.Sin(float64(cy)*0.12+float64(frameIdx)*0.35)))
		for cx := 0; cx < uvW; cx++ {
			sxU := cx - 2
			if sxU < 0 {
				sxU = 0
			}
			sxV := cx + 2
			if sxV >= uvW {
				sxV = uvW - 1
			}
			U[off+cx] = clampYUV(int32(origU[off+sxU])-4+wave, 32, 224)
			V[off+cx] = clampYUV(int32(origV[off+sxV])+5-wave, 32, 224)
		}
	}

	amp := 11.5
	switch maskIntensity & 3 {
	case MaskIntensitySubtle:
		amp = 9.0
	case MaskIntensityStrong:
		amp = 14.5
	case MaskIntensityMax:
		amp = 18.0
	}

	cols := dw / stealthVHSCellW
	rows := dh / stealthVHSCellH
	bitIdx := 0
	for r := 0; r < rows; r++ {
		py := y0 + r*stealthVHSCellH
		for c := 0; c < cols; c++ {
			px := x0 + c*stealthVHSCellW
			var bit byte
			if byteIdx := bitIdx >> 3; byteIdx < len(stream) {
				bit = (stream[byteIdx] >> uint(7-(bitIdx&7))) & 1
			} else {
				bit = byte(stealthMixHash(uint64(r), uint64(c), seed^uint64(frameIdx)) & 1)
			}
			bitIdx++
			sign := -1.0
			if bit == 1 {
				sign = 1.0
			}
			var sumT, sumB float64
			for dy := 0; dy < 4; dy++ {
				rowT := Y[(py+dy)*w+px : (py+dy)*w+px+stealthVHSCellW]
				rowB := Y[(py+4+dy)*w+px : (py+4+dy)*w+px+stealthVHSCellW]
				for dx := 0; dx < stealthVHSCellW; dx++ {
					sumT += float64(rowT[dx])
					sumB += float64(rowB[dx])
				}
			}
			diff := (sumT - sumB) / float64(4*stealthVHSCellW)
			adjT := -0.35*diff + sign*amp
			adjB := +0.35*diff - sign*amp
			for dy := 0; dy < 4; dy++ {
				rowT := Y[(py+dy)*w+px : (py+dy)*w+px+stealthVHSCellW]
				rowB := Y[(py+4+dy)*w+px : (py+4+dy)*w+px+stealthVHSCellW]
				for dx := 0; dx < stealthVHSCellW; dx++ {
					rowT[dx] = stealthClampY(float64(rowT[dx]) + adjT)
					rowB[dx] = stealthClampY(float64(rowB[dx]) + adjB)
				}
			}
		}
	}
	_ = h
}

func decodeStealthVHSGrid(src *YUVFrame, x0, y0, dw, dh, maxBytes int) []byte {
	w := src.Width
	Y := src.YPlane()
	cols := dw / stealthVHSCellW
	rows := dh / stealthVHSCellH
	out := make([]byte, maxBytes)
	maxBits := maxBytes * 8
	bitIdx := 0
	for r := 0; r < rows && bitIdx < maxBits; r++ {
		py := y0 + r*stealthVHSCellH
		for c := 0; c < cols && bitIdx < maxBits; c++ {
			px := x0 + c*stealthVHSCellW
			var sumT, sumB float64
			for dy := 0; dy < 4; dy++ {
				rowT := Y[(py+dy)*w+px : (py+dy)*w+px+stealthVHSCellW]
				rowB := Y[(py+4+dy)*w+px : (py+4+dy)*w+px+stealthVHSCellW]
				for dx := 2; dx < stealthVHSCellW-2; dx++ {
					sumT += float64(rowT[dx])
					sumB += float64(rowB[dx])
				}
			}
			if sumT >= sumB {
				out[bitIdx>>3] |= 1 << uint(7-(bitIdx&7))
			}
			bitIdx++
		}
	}
	return out
}


func decodeStealthVHSHeaderSlots(src *YUVFrame, w, h int) []byte {
	totalBytes := stealthTotalBytes("stealth-vhs", w, h)
	if totalBytes < stealthHeaderBytes {
		return nil
	}
	Y := src.YPlane()
	cols := w / stealthVHSCellW
	out := make([]byte, stealthHeaderBytes)
	for i := 0; i < stealthHeaderBytes; i++ {
		byteSlot := stealthHeaderBytePos(i, totalBytes)
		var b byte
		for bit := 0; bit < 8; bit++ {
			cellIdx := byteSlot*8 + bit
			r, c := cellIdx/cols, cellIdx%cols
			py, px := r*stealthVHSCellH, c*stealthVHSCellW
			var sumT, sumB float64
			for dy := 0; dy < 4; dy++ {
				rowT := Y[(py+dy)*w+px : (py+dy)*w+px+stealthVHSCellW]
				rowB := Y[(py+4+dy)*w+px : (py+4+dy)*w+px+stealthVHSCellW]
				for dx := 2; dx < stealthVHSCellW-2; dx++ {
					sumT += float64(rowT[dx])
					sumB += float64(rowB[dx])
				}
			}
			if sumT >= sumB {
				b |= 1 << uint(7-bit)
			}
		}
		out[i] = b
	}
	return out
}


// ----------------------------------------------------------------------------
// 2. STEALTH FALLING RAIN MASK (22x26 edge-to-edge diagonal PPM raindrops, 2 bits/drop)
// ----------------------------------------------------------------------------
func stealthRainLanePos(r, c, lane, phase int) (xTop, yTop int) {
	h := stealthMixHash(uint64(r+1000), uint64(c+1000), 0x5241494E)
	colStagger := uint64(((c*17 + r*7) % stealthRainCellH + stealthRainCellH) % stealthRainCellH)
	// Smooth 8-phase downward falling motion (~3.25px/frame across 26px cell height)
	stepY := uint64((phase * stealthRainCellH) / 8)
	yPhase := int((h>>8 + colStagger + stepY) % uint64(stealthRainCellH))
	xBase := 3 + lane*5 // 4 lanes at x = 3, 8, 13, 18 inside 22px cell (5px spacing)
	return xBase, yPhase - stealthRainLen/2
}

func encodeStealthRainGrid(dst *YUVFrame, x0, y0, dw, dh, phase int, seed uint64, stream []byte, maskIntensity uint8, fullScreen bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	// Slight overcast highlight roll-off guarantees bright raindrops stand out even over white clouds/logos!
	preMapLumaHeadroom(Y, w, x0, y0, dw, dh, 22, 204)

	// Subtle cool rainy atmosphere tint (skip top bar if !fullScreen)
	cyStart := 0
	if !fullScreen {
		cyStart = y0 / 2
	}
	uvW := w / 2
	for cy := cyStart; cy < h/2; cy++ {
		off := cy * uvW
		for cx := 0; cx < uvW; cx++ {
			U[off+cx] = clampYUV(int32(U[off+cx])+5, 24, 232)
			V[off+cx] = clampYUV(int32(V[off+cx])-3, 24, 232)
		}
	}

	amp := 38.0
	switch maskIntensity & 3 {
	case MaskIntensitySubtle:
		amp = 30.0
	case MaskIntensityStrong:
		amp = 48.0
	case MaskIntensityMax:
		amp = 58.0
	}

	cols := dw / stealthRainCellW
	rows := dh / stealthRainCellH
	bitIdx := 0
	nextBit := func(r, c int) byte {
		defer func() { bitIdx++ }()
		if byteIdx := bitIdx >> 3; byteIdx < len(stream) {
			return (stream[byteIdx] >> uint(7-(bitIdx&7))) & 1
		}
		return byte(stealthMixHash(uint64(r), uint64(c), seed^uint64(bitIdx)) & 1)
	}

	rMin, rMax := 0, rows-1
	cMin, cMax := 0, cols-1
	if fullScreen {
		// Sweep edge-to-edge including cosmetic outer droplets so rain covers y=0..h-1 and x=0..w-1!
		rMin, rMax = -1, rows
		cMin, cMax = 0, cols
	}
	for r := rMin; r <= rMax; r++ {
		py := y0 + r*stealthRainCellH
		for c := cMin; c <= cMax; c++ {
			px := x0 + c*stealthRainCellW
			isDataCell := r >= 0 && r < rows && c >= 0 && c < cols
			var sym int
			if isDataCell {
				sym = int((nextBit(r, c) << 1) | nextBit(r, c))
			} else {
				sym = int(stealthMixHash(uint64(r+100), uint64(c+100), seed^uint64(phase)) & 3)
			}

			for lane := 0; lane < 4; lane++ {
				lx, ly := stealthRainLanePos(r, c, lane, phase)
				for dy := 0; dy < stealthRainLen; dy++ {
					yy := py + ly + dy
					xx := px + lx - (dy / 4)
					if yy < y0 || yy >= y0+dh || xx < 0 || xx >= w {
						continue
					}
					if !fullScreen && (xx < x0 || xx >= x0+dw) {
						continue
					}
					row := Y[yy*w:]
					xL := xx - 2
					if xL < 0 {
						xL = 0
					}
					xR := xx + 3
					if xR >= w {
						xR = w - 1
					}
					flank := 0.5 * (float64(row[xL]) + float64(row[xR]))
					if lane == sym {
						taper := 0.55 + 0.45*math.Sin(math.Pi*float64(dy)/float64(stealthRainLen-1))
						target := flank + amp*taper
						if target > 234 {
							target = 234
						}
						row[xx] = stealthClampY(target)
						if xx+1 < w {
							row[xx+1] = stealthClampY(0.60*target + 0.40*float64(row[xx+1]))
						}
					} else if isDataCell && yy >= py && yy < py+stealthRainCellH && xx >= px+2 && xx < px+stealthRainCellW-2 {
						if float64(row[xx]) > flank+3.0 {
							row[xx] = stealthClampY(0.20*float64(row[xx]) + 0.80*flank)
							if xx+1 < w {
								row[xx+1] = stealthClampY(0.30*float64(row[xx+1]) + 0.70*flank)
							}
						}
					}
				}
			}
		}
	}
}

func decodeStealthRainGrid(src *YUVFrame, x0, y0, dw, dh, phase, maxBytes int) []byte {
	w, h := src.Width, src.Height
	Y := src.YPlane()
	cols := dw / stealthRainCellW
	rows := dh / stealthRainCellH
	out := make([]byte, maxBytes)
	maxBits := maxBytes * 8
	bitIdx := 0
	putBit := func(b byte) {
		if bitIdx < maxBits && b != 0 {
			out[bitIdx>>3] |= 1 << uint(7-(bitIdx&7))
		}
		bitIdx++
	}

	for r := 0; r < rows && bitIdx < maxBits; r++ {
		py := y0 + r*stealthRainCellH
		for c := 0; c < cols && bitIdx < maxBits; c++ {
			px := x0 + c*stealthRainCellW
			bestLane, bestScore := 0, -1e18
			for lane := 0; lane < 4; lane++ {
				lx, ly := stealthRainLanePos(r, c, lane, phase)
				var score, cnt float64
				for dy := 1; dy < stealthRainLen-1; dy++ {
					yy := py + ly + dy
					xx := px + lx - (dy / 4)
					if xx < 2 || xx >= w-3 || yy < y0 || yy >= y0+dh || yy < 0 || yy >= h {
						continue
					}
					row := Y[yy*w:]
					core := 0.65*float64(row[xx]) + 0.35*float64(row[xx+1])
					flank := 0.5 * (float64(row[xx-2]) + float64(row[xx+3]))
					score += core - flank
					cnt++
				}
				if cnt > 0 {
					score /= cnt
				}
				if score > bestScore {
					bestScore = score
					bestLane = lane
				}
			}
			putBit(byte((bestLane >> 1) & 1))
			putBit(byte(bestLane & 1))
		}
	}
	return out
}


func decodeStealthRainHeaderSlots(src *YUVFrame, w, h, phase int) []byte {
	totalBytes := stealthTotalBytes("stealth-rain", w, h)
	if totalBytes < stealthHeaderBytes {
		return nil
	}
	Y := src.YPlane()
	cols := w / stealthRainCellW
	out := make([]byte, stealthHeaderBytes)
	for i := 0; i < stealthHeaderBytes; i++ {
		byteSlot := stealthHeaderBytePos(i, totalBytes)
		var b byte
		for pair := 0; pair < 4; pair++ {
			cellIdx := byteSlot*4 + pair // 2 bits per raindrop cell -> 4 cells per byte
			r, c := cellIdx/cols, cellIdx%cols
			py, px := r*stealthRainCellH, c*stealthRainCellW
			bestLane, bestScore := 0, -1e18
			for lane := 0; lane < 4; lane++ {
				lx, ly := stealthRainLanePos(r, c, lane, phase)
				var score, cnt float64
				for dy := 1; dy < stealthRainLen-1; dy++ {
					yy := py + ly + dy
					xx := px + lx - (dy / 4)
					if xx < 2 || xx >= w-3 || yy < 0 || yy >= h {
						continue
					}
					row := Y[yy*w:]
					core := 0.65*float64(row[xx]) + 0.35*float64(row[xx+1])
					flank := 0.5 * (float64(row[xx-2]) + float64(row[xx+3]))
					score += core - flank
					cnt++
				}
				if cnt > 0 {
					score /= cnt
				}
				if score > bestScore {
					bestScore = score
					bestLane = lane
				}
			}
			b |= byte(bestLane) << uint(6-pair*2)
		}
		out[i] = b
	}
	return out
}


// ----------------------------------------------------------------------------
// 3. STEALTH 35mm CELLULOID FILM GRAIN (16x16 Hanning-windowed 4x4-clump DSSS)
// ----------------------------------------------------------------------------
func fillStealthFilmPatch(r, c, phase int, dst *[stealthFilmCell * stealthFilmCell]float64) {
	const g = stealthFilmCell / 4 // 4x4 coarse macro-grain lattice (4x4 px clumps -> survives H.264 & VP9!)
	var coarse [g * g]float64
	var mean float64
	for gy := 0; gy < g; gy++ {
		for gx := 0; gx < g; gx++ {
			h := stealthMixHash(uint64(r*97+gy), uint64(c*131+gx), uint64(phase)*31+0x354D4D)
			v := -1.0
			if h&1 == 1 {
				v = 1.0
			}
			coarse[gy*g+gx] = v
			mean += v
		}
	}
	mean /= float64(g * g)
	for i := range coarse {
		coarse[i] -= mean
	}
	var wMean float64
	for y := 0; y < stealthFilmCell; y++ {
		iy := stealthFilmIdx[y]
		wy0, wy1 := stealthFilmW0[y], stealthFilmW1[y]
		r0 := iy * g
		r1 := r0 + g
		dOff := y * stealthFilmCell
		for x := 0; x < stealthFilmCell; x++ {
			ix := stealthFilmIdx[x]
			wx0, wx1 := stealthFilmW0[x], stealthFilmW1[x]
			top := coarse[r0+ix]*wx0 + coarse[r0+ix+1]*wx1
			bot := coarse[r1+ix]*wx0 + coarse[r1+ix+1]*wx1
			val := (top*wy0 + bot*wy1) * stealthFilmWin[dOff+x]
			dst[dOff+x] = val
			wMean += val
		}
	}
	wMean *= 1.0 / float64(stealthFilmCell*stealthFilmCell)
	for i := range dst {
		dst[i] -= wMean
	}
}

func encodeStealthFilmGrid(dst *YUVFrame, x0, y0, dw, dh, phase int, seed uint64, stream []byte, maskIntensity uint8, fullScreen bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	// Authentic 35mm celluloid lifted blacks & soft highlights -> guarantees ±19 luma headroom on white logos & deep shadows!
	preMapLumaHeadroom(Y, w, x0, y0, dw, dh, 34, 216)

	// Classic warm Kodak/sepia cinema film tone
	cyStart := 0
	if !fullScreen {
		cyStart = y0 / 2
	}
	uvW := w / 2
	for cy := cyStart; cy < h/2; cy++ {
		off := cy * uvW
		for cx := 0; cx < uvW; cx++ {
			U[off+cx] = clampYUV(int32(U[off+cx])-9, 24, 232)
			V[off+cx] = clampYUV(int32(V[off+cx])+9, 24, 232)
		}
	}

	amp := 23.5
	maxGain := 74.0
	switch maskIntensity & 3 {
	case MaskIntensitySubtle:
		amp = 19.0
		maxGain = 60.0
	case MaskIntensityStrong:
		amp = 27.5
		maxGain = 86.0
	case MaskIntensityMax:
		amp = 31.5
		maxGain = 96.0
	}

	cols := dw / stealthFilmCell
	rows := dh / stealthFilmCell
	bitIdx := 0
	var patch [stealthFilmCell * stealthFilmCell]float64

	for r := 0; r < rows; r++ {
		py := y0 + r*stealthFilmCell
		for c := 0; c < cols; c++ {
			px := x0 + c*stealthFilmCell
			var bit byte
			if byteIdx := bitIdx >> 3; byteIdx < len(stream) {
				bit = (stream[byteIdx] >> uint(7-(bitIdx&7))) & 1
			} else {
				bit = byte(stealthMixHash(uint64(r), uint64(c), seed^uint64(phase)+9) & 1)
			}
			bitIdx++

			fillStealthFilmPatch(r, c, phase, &patch)
			var hostDot, norm2 float64
			for dy := 0; dy < stealthFilmCell; dy++ {
				row := Y[(py+dy)*w+px : (py+dy)*w+px+stealthFilmCell]
				pOff := dy * stealthFilmCell
				for dx := 0; dx < stealthFilmCell; dx++ {
					p := patch[pOff+dx]
					hostDot += float64(row[dx]) * p
					norm2 += p * p
				}
			}
			sign := -1.0
			if bit == 1 {
				sign = 1.0
			}
			curProj := hostDot / (norm2 + 1e-9)
			gain := sign*amp - curProj
			if gain > maxGain {
				gain = maxGain
			} else if gain < -maxGain {
				gain = -maxGain
			}
			for dy := 0; dy < stealthFilmCell; dy++ {
				row := Y[(py+dy)*w+px : (py+dy)*w+px+stealthFilmCell]
				pOff := dy * stealthFilmCell
				for dx := 0; dx < stealthFilmCell; dx++ {
					row[dx] = stealthClampY(float64(row[dx]) + gain*patch[pOff+dx])
				}
			}
		}
	}
}

func decodeStealthFilmGrid(src *YUVFrame, x0, y0, dw, dh, phase, maxBytes int) []byte {
	out, _ := decodeStealthFilmGridSoft(src, x0, y0, dw, dh, phase, maxBytes)
	return out
}


func decodeStealthFilmGridSoft(src *YUVFrame, x0, y0, dw, dh, phase, maxBytes int) ([]byte, []float32) {
	w := src.Width
	Y := src.YPlane()
	cols := dw / stealthFilmCell
	rows := dh / stealthFilmCell
	out := make([]byte, maxBytes)
	conf := make([]float32, maxBytes)
	for i := range conf {
		conf[i] = float32(1e9)
	}
	maxBits := maxBytes * 8
	bitIdx := 0
	var patch [stealthFilmCell * stealthFilmCell]float64

	for r := 0; r < rows && bitIdx < maxBits; r++ {
		py := y0 + r*stealthFilmCell
		for c := 0; c < cols && bitIdx < maxBits; c++ {
			px := x0 + c*stealthFilmCell
			fillStealthFilmPatch(r, c, phase, &patch)
			var dot, norm2 float64
			for dy := 0; dy < stealthFilmCell; dy++ {
				row := Y[(py+dy)*w+px : (py+dy)*w+px+stealthFilmCell]
				pOff := dy * stealthFilmCell
				for dx := 0; dx < stealthFilmCell; dx++ {
					p := patch[pOff+dx]
					dot += float64(row[dx]) * p
					norm2 += p * p
				}
			}
			bIdx := bitIdx >> 3
			if dot >= 0 {
				out[bIdx] |= 1 << uint(7-(bitIdx&7))
			}
			// Normalized matched-filter projection magnitude |dot| / sqrt(norm2):
			// tracks exact bit SNR so GMD erasure decoding flags the most uncertain bytes first!
			bitRel := float32(math.Abs(dot) / (math.Sqrt(norm2) + 1e-9))
			if bitRel < conf[bIdx] {
				conf[bIdx] = bitRel
			}
			bitIdx++
		}
	}
	return out, conf
}

func decodeStealthFilmHeaderSlots(src *YUVFrame, w, h, phase int) []byte {
	totalBytes := stealthTotalBytes("stealth-film", w, h)
	if totalBytes < stealthHeaderBytes {
		return nil
	}
	Y := src.YPlane()
	cols := w / stealthFilmCell
	out := make([]byte, stealthHeaderBytes)
	var patch [stealthFilmCell * stealthFilmCell]float64
	for i := 0; i < stealthHeaderBytes; i++ {
		byteSlot := stealthHeaderBytePos(i, totalBytes)
		var b byte
		for bit := 0; bit < 8; bit++ {
			cellIdx := byteSlot*8 + bit
			r, c := cellIdx/cols, cellIdx%cols
			py, px := r*stealthFilmCell, c*stealthFilmCell
			fillStealthFilmPatch(r, c, phase, &patch)
			var dot float64
			for dy := 0; dy < stealthFilmCell; dy++ {
				row := Y[(py+dy)*w+px : (py+dy)*w+px+stealthFilmCell]
				pOff := dy * stealthFilmCell
				for dx := 0; dx < stealthFilmCell; dx++ {
					dot += float64(row[dx]) * patch[pOff+dx]
				}
			}
			if dot >= 0 {
				b |= 1 << uint(7-bit)
			}
		}
		out[i] = b
	}
	return out
}

// DecodeStealthFilmWithBG rescues a ProfileStealthFilm frame damaged by YouTube VP9 inter-frame
// temporal attenuation (where VP9 preserves ~100% of a static/slow host H while attenuating the
// time-varying 35mm grain by beta, leaking (1-beta)*proj(H, P_t) into the matched filter!).
// Subtracting alpha*dot(BG, P_t) in unclamped float space cancels host leakage exactly and pairs
// with per-block GMD Soft-Decision Reed-Solomon decoding across multiple neighbor sets.
func DecodeStealthFilmWithBG(src *YUVFrame, neighbors [][]byte, expectedPhase int) (*DecodeFrameResult, error) {
	return DecodeStealthFilmWithNeighborSets(src, [][][]byte{neighbors}, expectedPhase)
}

// DecodeStealthFilmWithNeighborSets tries one or more temporal neighbor sets (e.g., rad7, prev5, next5,
// prev-keyframe t-1, tight2) and accumulates solved independent RS codewords across alphas & sets!
func DecodeStealthFilmWithNeighborSets(src *YUVFrame, neighborSets [][][]byte, expectedPhase int) (*DecodeFrameResult, error) {
	w, h := src.Width, src.Height
	Y := src.YPlane()
	phase := ((expectedPhase % 8) + 8) % 8
	cols := w / stealthFilmCell
	rows := h / stealthFilmCell
	numCells := cols * rows
	totalBytes := numCells / 8
	if totalBytes < stealthHeaderBytes {
		return nil, errors.New("resolução insuficiente para stealth-film")
	}

	// Precompute (dotY, invNorm) ONCE across all neighbor sets
	dotY := make([]float64, numCells)
	invNorm := make([]float64, numCells)
	patches := make([][stealthFilmCell * stealthFilmCell]float64, numCells)

	cellIdx := 0
	for r := 0; r < rows; r++ {
		py := r * stealthFilmCell
		for c := 0; c < cols; c++ {
			px := c * stealthFilmCell
			pPtr := &patches[cellIdx]
			fillStealthFilmPatch(r, c, phase, pPtr)
			var dySum, norm2 float64
			for dy := 0; dy < stealthFilmCell; dy++ {
				rowOff := (py+dy)*w + px
				pOff := dy * stealthFilmCell
				yRow := Y[rowOff : rowOff+stealthFilmCell]
				for dx := 0; dx < stealthFilmCell; dx++ {
					p := pPtr[pOff+dx]
					dySum += float64(yRow[dx]) * p
					norm2 += p * p
				}
			}
			dotY[cellIdx] = dySum
			invNorm[cellIdx] = 1.0 / (math.Sqrt(norm2) + 1e-9)
			cellIdx++
		}
	}

	baseProf := Profiles[ProfileStealthFilm]
	rs := GetRSCodec(baseProf.RSDataBytes)
	alphas := []float64{0.38, 0.34, 0.42, 0.30, 0.46, 0.25, 0.52, 0.20, 0.58, 0.14, 0.66, 0.08, 0.75, 0.0}

	var hdr FrameHeader
	hdrOK := false
	var rawLen, numBlocks int
	var hideHeader bool
	var solvedBlocks [][]byte
	var solvedErrs []int
	solvedCount := 0
	var lastErr error

	dotBG := make([]float64, numCells)
	full := make([]byte, totalBytes)
	fullConf := make([]float32, totalBytes)

	for _, neighbors := range neighborSets {
		if len(neighbors) == 0 {
			continue
		}
		invN := 1.0 / float64(len(neighbors))
		ci := 0
		for r := 0; r < rows; r++ {
			py := r * stealthFilmCell
			for c := 0; c < cols; c++ {
				px := c * stealthFilmCell
				pPtr := &patches[ci]
				var dbgSum float64
				for dy := 0; dy < stealthFilmCell; dy++ {
					rowOff := (py+dy)*w + px
					pOff := dy * stealthFilmCell
					for dx := 0; dx < stealthFilmCell; dx++ {
						var nSum float64
						for _, nb := range neighbors {
							nSum += float64(nb[rowOff+dx])
						}
						dbgSum += (nSum * invN) * pPtr[pOff+dx]
					}
				}
				dotBG[ci] = dbgSum
				ci++
			}
		}

		// 1. Decode hidden stealth header if not yet recovered
		if !hdrOK {
			for _, alpha := range append([]float64{0.0}, alphas[:len(alphas)-1]...) {
				raw96 := make([]byte, stealthHeaderBytes)
				for i := 0; i < stealthHeaderBytes; i++ {
					byteSlot := stealthHeaderBytePos(i, totalBytes)
					var b byte
					for bit := 0; bit < 8; bit++ {
						idx := byteSlot*8 + bit
						if dotY[idx]-alpha*dotBG[idx] >= 0 {
							b |= 1 << uint(7-bit)
						}
					}
					raw96[i] = b
				}
				if hCand, err := decodeStealthHeaderRS(raw96); err == nil && hCand.ProfileID == ProfileStealthFilm && int(hCand.FrameIndex%8) == phase {
					hdr = hCand
					hdrOK = true
					rawLen = baseProf.RawByteCapacityLayoutWithFlags(w, h, true, hdr.Flags)
					hideHeader = hdr.Flags&FlagHiddenHeader != 0
					numBlocks = rawLen / 255
					solvedBlocks = make([][]byte, numBlocks)
					solvedErrs = make([]int, numBlocks)
					break
				}
			}
			if !hdrOK {
				continue
			}
		}

		// 2. Sweep alpha in unclamped float space, solving ONLY remaining unsolved RS blocks!
		cw := make([]byte, 255)
		cwConf := make([]float32, 255)
		for _, alpha := range alphas {
			for bIdx := 0; bIdx < totalBytes; bIdx++ {
				var b byte
				minRel := float32(1e9)
				base := bIdx * 8
				for bit := 0; bit < 8; bit++ {
					idx := base + bit
					d := dotY[idx] - alpha*dotBG[idx]
					if d >= 0 {
						b |= 1 << uint(7-bit)
					}
					// Host-texture-aware reliability: down-weight cells where host projection |dotBG| is large!
					bgMag := math.Abs(dotBG[idx]) * invNorm[idx]
					rel := float32((math.Abs(d) * invNorm[idx]) / (1.0 + 0.03*bgMag))
					if rel < minRel {
						minRel = rel
					}
				}
				full[bIdx] = b
				fullConf[bIdx] = minRel
			}
			raw, rawConf := extractStealthPayloadWithConf(full, fullConf, totalBytes, rawLen, hideHeader)
			for b := 0; b < numBlocks; b++ {
				if solvedBlocks[b] != nil {
					continue
				}
				for j := 0; j < 255; j++ {
					off := j*numBlocks + b
					cw[j] = raw[off]
					cwConf[j] = rawConf[off]
				}
				corrected, err := rs.DecodeBlockSoft(cw, cwConf)
				if err == nil {
					data := make([]byte, rs.DataBytes)
					copy(data, cw[:rs.DataBytes])
					solvedBlocks[b] = data
					solvedErrs[b] = corrected
					solvedCount++
				} else {
					lastErr = fmt.Errorf("bloco RS %d/%d irrecuperável: %w", b, numBlocks, err)
				}
			}
			if solvedCount == numBlocks {
				payload := make([]byte, numBlocks*rs.DataBytes)
				totCorr := 0
				for b := 0; b < numBlocks; b++ {
					copy(payload[b*rs.DataBytes:(b+1)*rs.DataBytes], solvedBlocks[b])
					totCorr += solvedErrs[b]
				}
				if int(hdr.PayloadLen) > len(payload) {
					return nil, fmt.Errorf("frame %d payload_len inválido (%d > %d)", hdr.FrameIndex, hdr.PayloadLen, len(payload))
				}
				return &DecodeFrameResult{
					Header:         hdr,
					HeaderKind:     HeaderKindHiddenStealth,
					Payload:        payload[:hdr.PayloadLen],
					CorrectedBytes: totCorr,
					RawByteCount:   rawLen,
				}, nil
			}
		}
	}
	if !hdrOK {
		return nil, errors.New("cabeçalho stealth-film irrecuperável mesmo com cancelamento temporal")
	}
	return nil, fmt.Errorf("frame %d falhou no resgate temporal (%d/%d blocos RS recuperados): %w", hdr.FrameIndex, solvedCount, numBlocks, lastErr)
}


// ----------------------------------------------------------------------------
// 4. STEALTH STARRY MASK — FULL-SCREEN TRANSPARENT STARLIGHT OVERLAY (32x30, 3 bits/star!)
//    Pure twinkling stars directly over ANY host video or art — NO synthetic sky/mountains!
// ----------------------------------------------------------------------------
func stealthStarCandidatePos(r, c, sym int) (int, int) {
	baseX := [8]int{5, 13, 20, 27, 6, 14, 21, 26}
	baseY := [8]int{6, 7, 6, 8, 21, 22, 21, 23}
	h := stealthMixHash(uint64(r*37+sym), uint64(c*53+sym), 0x53544152)
	jx := int(h%3) - 1
	jy := int((h>>4)%3) - 1
	x := baseX[sym] + jx
	y := baseY[sym] + jy
	if x < 4 {
		x = 4
	} else if x > stealthStarCellW-5 {
		x = stealthStarCellW - 5
	}
	if y < 4 {
		y = 4
	} else if y > stealthStarCellH-5 {
		y = stealthStarCellH - 5
	}
	return x, y
}

func stealthStarRingMean(Y []byte, w, cx, cy int) float64 {
	return (float64(Y[(cy-3)*w+cx]) + float64(Y[(cy+3)*w+cx]) +
		float64(Y[cy*w+(cx-3)]) + float64(Y[cy*w+(cx+3)]) +
		float64(Y[(cy-3)*w+(cx-3)]) + float64(Y[(cy-3)*w+(cx+3)]) +
		float64(Y[(cy+3)*w+(cx-3)]) + float64(Y[(cy+3)*w+(cx+3)])) / 8.0
}

func stealthStarCoreMean(Y []byte, w, cx, cy int) float64 {
	var sum float64
	for dy := -1; dy <= 1; dy++ {
		off := (cy + dy) * w
		for dx := -1; dx <= 1; dx++ {
			sum += float64(Y[off+cx+dx])
		}
	}
	return sum / 9.0
}

func encodeStealthStarsGrid(dst *YUVFrame, x0, y0, dw, dh, frameIdx int, seed uint64, stream []byte, maskIntensity uint8, fullScreen bool) {
	w, h := dst.Width, dst.Height
	Y, U, V := dst.YPlane(), dst.UPlane(), dst.VPlane()
	uvW := w / 2
	cols := dw / stealthStarCellW
	rows := dh / stealthStarCellH

	minBoost := 68.0
	peakBase := 95.0
	switch maskIntensity & 3 {
	case MaskIntensitySubtle:
		minBoost = 54.0
		peakBase = 76.0
	case MaskIntensityStrong:
		minBoost = 82.0
		peakBase = 115.0
	case MaskIntensityMax:
		minBoost = 98.0
		peakBase = 135.0
	}

	bitIdx := 0
	nextBit := func(r, c int) byte {
		defer func() { bitIdx++ }()
		if byteIdx := bitIdx >> 3; byteIdx < len(stream) {
			return (stream[byteIdx] >> uint(7-(bitIdx&7))) & 1
		}
		return byte(stealthMixHash(uint64(r), uint64(c), seed^uint64(bitIdx)+77) & 1)
	}

	for r := 0; r < rows; r++ {
		py := y0 + r*stealthStarCellH
		for c := 0; c < cols; c++ {
			px := x0 + c*stealthStarCellW
			sym := int((nextBit(r, c) << 2) | (nextBit(r, c) << 1) | nextBit(r, c))

			// 1. Gently suppress accidental bright host dots at the 7 non-winning candidates
			for k := 0; k < 8; k++ {
				if k == sym {
					continue
				}
				sx, sy := stealthStarCandidatePos(r, c, k)
				cx, cy := px+sx, py+sy
				ring := stealthStarRingMean(Y, w, cx, cy)
				core := stealthStarCoreMean(Y, w, cx, cy)
				if core > ring+3.0 {
					for dy := -1; dy <= 1; dy++ {
						off := (cy + dy) * w
						for dx := -1; dx <= 1; dx++ {
							v := float64(Y[off+cx+dx])
							if v > ring+2.0 {
								Y[off+cx+dx] = stealthClampY(0.20*v + 0.80*ring)
							}
						}
					}
				}
			}

			// 2. Stamp crisp twinkling star at winning candidate (works over dark AND bright host videos!)
			sx, sy := stealthStarCandidatePos(r, c, sym)
			cx, cy := px+sx, py+sy
			ring := stealthStarRingMean(Y, w, cx, cy)

			if ring > 160.0 {
				drop := ring - 155.0
				for dy := -3; dy <= 3; dy++ {
					yy := cy + dy
					if yy < y0 || yy >= y0+dh {
						continue
					}
					for dx := -3; dx <= 3; dx++ {
						xx := cx + dx
						if xx < x0 || xx >= x0+dw {
							continue
						}
						d2 := float64(dx*dx + dy*dy)
						if d2 >= 2.0 && d2 <= 12.0 {
							wHalo := math.Exp(-math.Pow(math.Sqrt(d2)-2.6, 2.0) / 1.8)
							Y[yy*w+xx] = stealthClampY(float64(Y[yy*w+xx]) - drop*wHalo)
						}
					}
				}
				ring = stealthStarRingMean(Y, w, cx, cy)
			}

			sh := stealthMixHash(uint64(r), uint64(c), seed^uint64(frameIdx/2))
			peak := peakBase + float64(sh%50)
			targetPeak := ring + peak
			if targetPeak > 235.0 {
				targetPeak = 235.0
			}
			boost := targetPeak - ring
			if boost < minBoost {
				boost = minBoost
			}
			radius := 1.25 + float64((sh>>8)%6)*0.06
			inv2Sig := 1.0 / (2.0 * radius * radius)

			for dy := -2; dy <= 2; dy++ {
				yy := cy + dy
				if yy < 0 || yy >= h {
					continue
				}
				for dx := -2; dx <= 2; dx++ {
					xx := cx + dx
					if xx < 0 || xx >= w {
						continue
					}
					d2 := float64(dx*dx + dy*dy)
					glow := boost * math.Exp(-d2*inv2Sig)
					if (dx == 0 && (dy == -2 || dy == 2)) || (dy == 0 && (dx == -2 || dx == 2)) {
						glow *= 1.35
					}
					base := float64(Y[yy*w+xx])
					desired := ring + glow
					if desired > base {
						Y[yy*w+xx] = stealthClampY(desired)
					}
				}
			}

			// Subtle star sparkle color ONLY on the star pixel itself (host colors untouched!)
			tint := (sh >> 16) % 3
			ucx, ucy := cx>>1, cy>>1
			if ucx >= 0 && ucx < uvW && ucy >= 0 && ucy < h/2 {
				switch tint {
				case 0:
					U[ucy*uvW+ucx] = 114
					V[ucy*uvW+ucx] = 142
				case 1:
					U[ucy*uvW+ucx] = 148
					V[ucy*uvW+ucx] = 122
				default:
					U[ucy*uvW+ucx] = 128
					V[ucy*uvW+ucx] = 128
				}
			}
		}
	}
	_ = fullScreen
}

func decodeStealthStarsGrid(src *YUVFrame, x0, y0, dw, dh, maxBytes int) []byte {
	w := src.Width
	Y := src.YPlane()
	cols := dw / stealthStarCellW
	rows := dh / stealthStarCellH
	out := make([]byte, maxBytes)
	maxBits := maxBytes * 8
	bitIdx := 0
	putBit := func(b byte) {
		if bitIdx < maxBits && b != 0 {
			out[bitIdx>>3] |= 1 << uint(7-(bitIdx&7))
		}
		bitIdx++
	}

	for r := 0; r < rows && bitIdx < maxBits; r++ {
		py := y0 + r*stealthStarCellH
		for c := 0; c < cols && bitIdx < maxBits; c++ {
			px := x0 + c*stealthStarCellW
			bestSym, bestScore := 0, -1e18
			for sym := 0; sym < 8; sym++ {
				sx, sy := stealthStarCandidatePos(r, c, sym)
				cx, cy := px+sx, py+sy
				score := stealthStarCoreMean(Y, w, cx, cy) - stealthStarRingMean(Y, w, cx, cy)
				if score > bestScore {
					bestScore = score
					bestSym = sym
				}
			}
			putBit(byte((bestSym >> 2) & 1))
			putBit(byte((bestSym >> 1) & 1))
			putBit(byte(bestSym & 1))
		}
	}
	return out
}


func decodeStealthStarsHeaderSlots(src *YUVFrame, w, h int) []byte {
	totalBytes := stealthTotalBytes("stealth-stars", w, h)
	if totalBytes < stealthHeaderBytes {
		return nil
	}
	Y := src.YPlane()
	cols := w / stealthStarCellW
	out := make([]byte, stealthHeaderBytes)
	evalCell := func(cellIdx int) int {
		r, c := cellIdx/cols, cellIdx%cols
		py, px := r*stealthStarCellH, c*stealthStarCellW
		bestSym, bestScore := 0, -1e18
		for sym := 0; sym < 8; sym++ {
			sx, sy := stealthStarCandidatePos(r, c, sym)
			cx, cy := px+sx, py+sy
			score := stealthStarCoreMean(Y, w, cx, cy) - stealthStarRingMean(Y, w, cx, cy)
			if score > bestScore {
				bestScore = score
				bestSym = sym
			}
		}
		return bestSym
	}
	cacheIdx, cacheSym := -1, 0
	for i := 0; i < stealthHeaderBytes; i++ {
		byteSlot := stealthHeaderBytePos(i, totalBytes)
		var b byte
		for bit := 0; bit < 8; bit++ {
			gBit := byteSlot*8 + bit
			cellIdx := gBit / 3
			bitInCell := gBit % 3 // 0 = MSB, 1 = mid, 2 = LSB
			if cellIdx != cacheIdx {
				cacheIdx = cellIdx
				cacheSym = evalCell(cellIdx)
			}
			if (cacheSym>>uint(2-bitInCell))&1 != 0 {
				b |= 1 << uint(7-bit)
			}
		}
		out[i] = b
	}
	return out
}


package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	FrameHeaderMagic   = "YTC1" // Legacy V1 layout (160px top/bottom, 48px sides, 4 QRs)
	FrameHeaderMagicV2 = "YTC2" // Full-Frame V2 layout (96.2% 4K area: 32px top bar, 16px borders, 8x8 DCT header)
	FrameHeaderSize    = 32
	HeaderBlockSize    = 12 // V1 12x12 px P&B blocks
	HeaderBlockSizeV2  = 8  // V2 8x8 px DCT-aligned P&B blocks
	HeaderCopies       = 3

	FlagManifest       uint8 = 1 << 0
	FlagLastFrame      uint8 = 1 << 1
	FlagHiddenHeader   uint8 = 1 << 2 // Seamless full-frame header (no B&W top bar!)
	FlagMaskShift      uint8 = 3      // Bits 3..4 encode MaskIntensity (0..3)
	FlagHostManchester uint8 = 1 << 5 // Blind Manchester luma coding over an external host video (art-stego + host)
	FlagSpread         uint8 = 1 << 6 // Data frames spread evenly across mandatory video duration (stride stored in manifest)
	FlagVisualFiller   uint8 = 1 << 7 // Visual continuation frame (matches optical texture, no YTC2 header written!)

	MaskIntensityDefault uint8 = 0 // Equilibrada (1x dados/frame — padrão)
	MaskIntensitySubtle  uint8 = 1 // Sutil (0.25x dados/frame — vídeo/arte mais limpo)
	MaskIntensityStrong  uint8 = 2 // Forte (2x dados/frame — mais máscara, 2x GB)
	MaskIntensityMax     uint8 = 3 // Máxima (4x dados/frame — máscara intensa, 4x GB)
)

// MaskIntensityFromFlags extracts the 2-bit MaskIntensity (0..3) from FrameHeader.Flags.
func MaskIntensityFromFlags(flags uint8) uint8 {
	return (flags >> FlagMaskShift) & 3
}

// BuildHeaderFlags encodes MaskIntensity (0..3) and HiddenHeader into FrameHeader.Flags bits.
func BuildHeaderFlags(maskIntensity uint8, hiddenHeader bool) uint8 {
	f := (maskIntensity & 3) << FlagMaskShift
	if hiddenHeader {
		f |= FlagHiddenHeader
	}
	return f
}

// ParseMaskIntensity maps UI string codes ("subtle", "balanced", "strong", "max") to MaskIntensity 0..3.
func ParseMaskIntensity(code string) uint8 {
	switch code {
	case "subtle", "sutil", "1":
		return MaskIntensitySubtle
	case "strong", "forte", "2":
		return MaskIntensityStrong
	case "max", "maxima", "máxima", "3":
		return MaskIntensityMax
	default:
		return MaskIntensityDefault
	}
}

// Studio-safe BT.709/BT.601 YUV420p reference levels (avoids [0..15] and [236..255] clipping)
var (
	RefYLevels  = [4]byte{24, 96, 168, 235}
	RefY6Levels = [6]byte{24, 66, 108, 151, 193, 235}
	RefY8Levels = [8]byte{24, 54, 84, 114, 145, 175, 205, 235}
	RefCbLevels = [2]byte{64, 192}
	RefCrLevels = [2]byte{64, 192}

	gray2ToLevel = [4]int{0, 1, 3, 2}
	levelToGray2 = [4]byte{0, 1, 3, 2}
	gray3ToLevel = [8]int{0, 1, 3, 2, 7, 6, 4, 5} // inverse of lvl ^ (lvl >> 1)
)

type FrameHeader struct {
	Version     uint8
	ProfileID   ProfileID
	Flags       uint8
	ArtTheme    uint8 // 0=Auto, 1=Aurora, 2=Winamp, 3=Lightning, 4=Vortex, 5=Beach, 6=Flora
	ContainerID uint64
	FrameIndex  uint32
	TotalFrames uint32
	PayloadLen  uint32
}

func (h FrameHeader) Marshal() [FrameHeaderSize]byte {
	var buf [FrameHeaderSize]byte
	if h.Version >= 2 {
		copy(buf[0:4], FrameHeaderMagicV2)
	} else {
		copy(buf[0:4], FrameHeaderMagic)
	}
	buf[4] = h.Version
	buf[5] = byte(h.ProfileID)
	buf[6] = h.Flags
	buf[7] = h.ArtTheme
	binary.LittleEndian.PutUint64(buf[8:16], h.ContainerID)
	binary.LittleEndian.PutUint32(buf[16:20], h.FrameIndex)
	binary.LittleEndian.PutUint32(buf[20:24], h.TotalFrames)
	binary.LittleEndian.PutUint32(buf[24:28], h.PayloadLen)
	crc := crc32.ChecksumIEEE(buf[0:28])
	binary.LittleEndian.PutUint32(buf[28:32], crc)
	return buf
}

func UnmarshalFrameHeader(b []byte) (FrameHeader, error) {
	if len(b) < FrameHeaderSize {
		return FrameHeader{}, errors.New("cabeçalho curto")
	}
	magic := string(b[0:4])
	if magic != FrameHeaderMagic && magic != FrameHeaderMagicV2 {
		return FrameHeader{}, errors.New("assinatura mágica YTC1/YTC2 ausente")
	}
	expected := binary.LittleEndian.Uint32(b[28:32])
	actual := crc32.ChecksumIEEE(b[0:28])
	if expected != actual {
		return FrameHeader{}, errors.New("CRC32 do cabeçalho inválido")
	}
	return FrameHeader{
		Version:     b[4],
		ProfileID:   ProfileID(b[5]),
		Flags:       b[6],
		ArtTheme:    b[7],
		ContainerID: binary.LittleEndian.Uint64(b[8:16]),
		FrameIndex:  binary.LittleEndian.Uint32(b[16:20]),
		TotalFrames: binary.LittleEndian.Uint32(b[20:24]),
		PayloadLen:  binary.LittleEndian.Uint32(b[24:28]),
	}, nil
}

// YUVFrame holds a raw yuv420p frame buffer (Y, U=Cb, V=Cr).
type YUVFrame struct {
	Width  int
	Height int
	Buf    []byte // len = W*H*3/2
}

func NewYUVFrame(w, h int) *YUVFrame {
	return &YUVFrame{
		Width:  w,
		Height: h,
		Buf:    make([]byte, w*h*3/2),
	}
}

func (f *YUVFrame) YPlane() []byte { return f.Buf[:f.Width*f.Height] }
func (f *YUVFrame) UPlane() []byte {
	yLen := f.Width * f.Height
	uvLen := (f.Width / 2) * (f.Height / 2)
	return f.Buf[yLen : yLen+uvLen]
}
func (f *YUVFrame) VPlane() []byte {
	yLen := f.Width * f.Height
	uvLen := (f.Width / 2) * (f.Height / 2)
	return f.Buf[yLen+uvLen:]
}

func (f *YUVFrame) ClearNeutral() {
	y := f.YPlane()
	for i := range y {
		y[i] = 24 // dark clean border background
	}
	u := f.UPlane()
	for i := range u {
		u[i] = 128
	}
	v := f.VPlane()
	for i := range v {
		v[i] = 128
	}
}

func (f *YUVFrame) fillYRect(x0, y0, w, h int, val byte) {
	Y := f.YPlane()
	stride := f.Width
	for yy := y0; yy < y0+h; yy++ {
		row := Y[yy*stride+x0 : yy*stride+x0+w]
		for i := range row {
			row[i] = val
		}
	}
}

func (f *YUVFrame) fillUVRect(x0, y0, w, h int, cb, cr byte) {
	U := f.UPlane()
	V := f.VPlane()
	stride := f.Width / 2
	cx0, cy0 := x0/2, y0/2
	cw, ch := w/2, h/2
	for yy := cy0; yy < cy0+ch; yy++ {
		off := yy*stride + cx0
		uRow := U[off : off+cw]
		vRow := V[off : off+cw]
		for i := range uRow {
			uRow[i] = cb
			vRow[i] = cr
		}
	}
}

func (f *YUVFrame) drawFinder(x0, y0 int) {
	// 96x96 QR-style concentric finder pattern (outer 96 white, 72 black, 48 white, 24 black)
	f.fillYRect(x0, y0, 96, 96, 235)
	f.fillYRect(x0+12, y0+12, 72, 72, 24)
	f.fillYRect(x0+24, y0+24, 48, 48, 235)
	f.fillYRect(x0+36, y0+36, 24, 24, 24)
	f.fillUVRect(x0, y0, 96, 96, 128, 128)
}

func (f *YUVFrame) drawCalibrationStrip() {
	// Located at x = 168..360 (8 strips of 24x96), y = 48..144
	for i := 0; i < 4; i++ {
		x := 168 + i*24
		f.fillYRect(x, FrameMargin, 24, HeaderHeight, RefYLevels[i])
		f.fillUVRect(x, FrameMargin, 24, HeaderHeight, 128, 128)
	}
	for i := 0; i < 2; i++ {
		x := 264 + i*24
		f.fillYRect(x, FrameMargin, 24, HeaderHeight, 128)
		f.fillUVRect(x, FrameMargin, 24, HeaderHeight, RefCbLevels[i], 128)
	}
	for i := 0; i < 2; i++ {
		x := 312 + i*24
		f.fillYRect(x, FrameMargin, 24, HeaderHeight, 128)
		f.fillUVRect(x, FrameMargin, 24, HeaderHeight, 128, RefCrLevels[i])
	}
}

func avgPlaneRect(plane []byte, stride, x0, y0, w, h int) float64 {
	sum := 0
	for yy := y0; yy < y0+h; yy++ {
		off := yy*stride + x0
		for xx := 0; xx < w; xx++ {
			sum += int(plane[off+xx])
		}
	}
	return float64(sum) / float64(w*h)
}

type calThresholds struct {
	yThresh2 float64
	yThresh4 [3]float64
	yThresh6 [5]float64
	yThresh8 [7]float64
	cbThresh float64
	crThresh float64
}

func (f *YUVFrame) readCalibration() calThresholds {
	Y := f.YPlane()
	U := f.UPlane()
	V := f.VPlane()
	w := f.Width
	uvStride := w / 2

	var refY [4]float64
	for i := 0; i < 4; i++ {
		x := 168 + i*24
		refY[i] = avgPlaneRect(Y, w, x+6, FrameMargin+16, 12, HeaderHeight-32)
	}
	var refCb [2]float64
	for i := 0; i < 2; i++ {
		x := (264 + i*24) / 2
		refCb[i] = avgPlaneRect(U, uvStride, x+3, (FrameMargin+16)/2, 6, (HeaderHeight-32)/2)
	}
	var refCr [2]float64
	for i := 0; i < 2; i++ {
		x := (312 + i*24) / 2
		refCr[i] = avgPlaneRect(V, uvStride, x+3, (FrameMargin+16)/2, 6, (HeaderHeight-32)/2)
	}

	cal := calThresholds{
		yThresh2: 129.5,
		yThresh4: [3]float64{60.0, 132.0, 201.5},
		yThresh6: [5]float64{45.1, 87.3, 129.5, 171.7, 213.9},
		yThresh8: [7]float64{39.1, 69.2, 99.4, 129.5, 159.6, 189.8, 219.9},
		cbThresh: 128.0,
		crThresh: 128.0,
	}
	// Accept measured reference values if monotonic with healthy separation
	if refY[3]-refY[0] > 80 {
		lo, hi := refY[0], refY[3]
		cal.yThresh2 = (lo + hi) / 2.0
		span := hi - lo
		for k := 0; k < 5; k++ {
			cal.yThresh6[k] = lo + span*(float64(k)+0.5)/5.0
		}
		for k := 0; k < 7; k++ {
			cal.yThresh8[k] = lo + span*(float64(k)+0.5)/7.0
		}
	}
	if refY[1]-refY[0] > 25 && refY[2]-refY[1] > 25 && refY[3]-refY[2] > 25 {
		cal.yThresh4[0] = (refY[0] + refY[1]) / 2.0
		cal.yThresh4[1] = (refY[1] + refY[2]) / 2.0
		cal.yThresh4[2] = (refY[2] + refY[3]) / 2.0
	}
	if refCb[1]-refCb[0] > 40 {
		cal.cbThresh = (refCb[0] + refCb[1]) / 2.0
	}
	if refCr[1]-refCr[0] > 40 {
		cal.crThresh = (refCr[0] + refCr[1]) / 2.0
	}
	return cal
}

func (f *YUVFrame) drawHeaderBits(hdr FrameHeader) {
	hb := hdr.Marshal()
	// 3 copies = 96 bytes = 768 bits, written at x=384..W-168, y=48..144 in 12x12 blocks
	xStart := 384
	cols := (f.Width - 168 - xStart) / HeaderBlockSize
	bitIdx := 0
	for copyIdx := 0; copyIdx < HeaderCopies; copyIdx++ {
		for _, b := range hb {
			for bit := 7; bit >= 0; bit-- {
				r := bitIdx / cols
				c := bitIdx % cols
				val := RefYLevels[0]
				if (b>>uint(bit))&1 == 1 {
					val = RefYLevels[3]
				}
				f.fillYRect(xStart+c*HeaderBlockSize, FrameMargin+r*HeaderBlockSize, HeaderBlockSize, HeaderBlockSize, val)
				bitIdx++
			}
		}
	}
}

func (f *YUVFrame) readHeader(cal calThresholds) (FrameHeader, error) {
	Y := f.YPlane()
	w := f.Width
	xStart := 384
	cols := (w - 168 - xStart) / HeaderBlockSize
	var copies [HeaderCopies][FrameHeaderSize]byte

	bitIdx := 0
	for copyIdx := 0; copyIdx < HeaderCopies; copyIdx++ {
		for byteIdx := 0; byteIdx < FrameHeaderSize; byteIdx++ {
			var b byte
			for bit := 7; bit >= 0; bit-- {
				r := bitIdx / cols
				c := bitIdx % cols
				avg := avgPlaneRect(Y, w, xStart+c*HeaderBlockSize+3, FrameMargin+r*HeaderBlockSize+3, 6, 6)
				if avg >= cal.yThresh2 {
					b |= 1 << uint(bit)
				}
				bitIdx++
			}
			copies[copyIdx][byteIdx] = b
		}
	}

	// 1. Try each copy directly
	for i := 0; i < HeaderCopies; i++ {
		if h, err := UnmarshalFrameHeader(copies[i][:]); err == nil {
			return h, nil
		}
	}
	// 2. Bitwise majority vote across the 3 copies
	var voted [FrameHeaderSize]byte
	for byteIdx := 0; byteIdx < FrameHeaderSize; byteIdx++ {
		var b byte
		for bit := 0; bit < 8; bit++ {
			ones := 0
			for i := 0; i < HeaderCopies; i++ {
				if (copies[i][byteIdx]>>uint(bit))&1 == 1 {
					ones++
				}
			}
			if ones >= 2 {
				b |= 1 << uint(bit)
			}
		}
		voted[byteIdx] = b
	}
	return UnmarshalFrameHeader(voted[:])
}

func (f *YUVFrame) drawCalibrationStripV2() {
	// Ultra-compact V2 calibration strip: x = 16..144 (8 strips of 16x32), y = 16..48 (exact 16x16 macroblocks!)
	for i := 0; i < 4; i++ {
		x := FrameMarginV2 + i*16
		f.fillYRect(x, HeaderTopV2, 16, HeaderHeightV2, RefYLevels[i])
		f.fillUVRect(x, HeaderTopV2, 16, HeaderHeightV2, 128, 128)
	}
	for i := 0; i < 2; i++ {
		x := FrameMarginV2 + (4+i)*16
		f.fillYRect(x, HeaderTopV2, 16, HeaderHeightV2, 128)
		f.fillUVRect(x, HeaderTopV2, 16, HeaderHeightV2, RefCbLevels[i], 128)
	}
	for i := 0; i < 2; i++ {
		x := FrameMarginV2 + (6+i)*16
		f.fillYRect(x, HeaderTopV2, 16, HeaderHeightV2, 128)
		f.fillUVRect(x, HeaderTopV2, 16, HeaderHeightV2, 128, RefCrLevels[i])
	}
}

func buildCalThresholds(refY [4]float64, refCb, refCr [2]float64) calThresholds {
	cal := calThresholds{
		yThresh2: 129.5,
		yThresh4: [3]float64{60.0, 132.0, 201.5},
		yThresh6: [5]float64{45.1, 87.3, 129.5, 171.7, 213.9},
		yThresh8: [7]float64{39.1, 69.2, 99.4, 129.5, 159.6, 189.8, 219.9},
		cbThresh: 128.0,
		crThresh: 128.0,
	}
	if refY[3]-refY[0] > 80 {
		lo, hi := refY[0], refY[3]
		cal.yThresh2 = (lo + hi) / 2.0
		span := hi - lo
		for k := 0; k < 5; k++ {
			cal.yThresh6[k] = lo + span*(float64(k)+0.5)/5.0
		}
		for k := 0; k < 7; k++ {
			cal.yThresh8[k] = lo + span*(float64(k)+0.5)/7.0
		}
	}
	if refY[1]-refY[0] > 25 && refY[2]-refY[1] > 25 && refY[3]-refY[2] > 25 {
		cal.yThresh4[0] = (refY[0] + refY[1]) / 2.0
		cal.yThresh4[1] = (refY[1] + refY[2]) / 2.0
		cal.yThresh4[2] = (refY[2] + refY[3]) / 2.0
	}
	if refCb[1]-refCb[0] > 40 {
		cal.cbThresh = (refCb[0] + refCb[1]) / 2.0
	}
	if refCr[1]-refCr[0] > 40 {
		cal.crThresh = (refCr[0] + refCr[1]) / 2.0
	}
	return cal
}

func (f *YUVFrame) readCalibrationV2() calThresholds {
	Y := f.YPlane()
	U := f.UPlane()
	V := f.VPlane()
	w := f.Width
	uvStride := w / 2

	var refY [4]float64
	for i := 0; i < 4; i++ {
		x := FrameMarginV2 + i*16
		refY[i] = avgPlaneRect(Y, w, x+4, HeaderTopV2+8, 8, 16)
	}
	var refCb [2]float64
	for i := 0; i < 2; i++ {
		x := (FrameMarginV2 + (4+i)*16) / 2
		refCb[i] = avgPlaneRect(U, uvStride, x+2, (HeaderTopV2+8)/2, 4, 8)
	}
	var refCr [2]float64
	for i := 0; i < 2; i++ {
		x := (FrameMarginV2 + (6+i)*16) / 2
		refCr[i] = avgPlaneRect(V, uvStride, x+2, (HeaderTopV2+8)/2, 4, 8)
	}
	return buildCalThresholds(refY, refCb, refCr)
}

func (f *YUVFrame) drawHeaderBitsV2(hdr FrameHeader) {
	hb := hdr.Marshal()
	// 3 copies = 96 bytes = 768 bits, written at x=160..W-16, y=16..48 in exact 8x8 DCT-aligned P&B blocks!
	xStart := 160
	cols := (f.Width - FrameMarginV2 - xStart) / HeaderBlockSizeV2
	bitIdx := 0
	for copyIdx := 0; copyIdx < HeaderCopies; copyIdx++ {
		for _, b := range hb {
			for bit := 7; bit >= 0; bit-- {
				r := bitIdx / cols
				c := bitIdx % cols
				val := RefYLevels[0]
				if (b>>uint(bit))&1 == 1 {
					val = RefYLevels[3]
				}
				f.fillYRect(xStart+c*HeaderBlockSizeV2, HeaderTopV2+r*HeaderBlockSizeV2, HeaderBlockSizeV2, HeaderBlockSizeV2, val)
				bitIdx++
			}
		}
	}
}

func (f *YUVFrame) readHeaderV2(cal calThresholds) (FrameHeader, error) {
	Y := f.YPlane()
	w := f.Width
	xStart := 160
	cols := (w - FrameMarginV2 - xStart) / HeaderBlockSizeV2
	if cols <= 0 {
		return FrameHeader{}, errors.New("largura insuficiente para cabeçalho V2")
	}
	var copies [HeaderCopies][FrameHeaderSize]byte
	bitIdx := 0
	for copyIdx := 0; copyIdx < HeaderCopies; copyIdx++ {
		for byteIdx := 0; byteIdx < FrameHeaderSize; byteIdx++ {
			var b byte
			for bit := 7; bit >= 0; bit-- {
				r := bitIdx / cols
				c := bitIdx % cols
				avg := avgPlaneRect(Y, w, xStart+c*HeaderBlockSizeV2+2, HeaderTopV2+r*HeaderBlockSizeV2+2, 4, 4)
				if avg >= cal.yThresh2 {
					b |= 1 << uint(bit)
				}
				bitIdx++
			}
			copies[copyIdx][byteIdx] = b
		}
	}
	for i := 0; i < HeaderCopies; i++ {
		if h, err := UnmarshalFrameHeader(copies[i][:]); err == nil && h.Version >= 2 {
			return h, nil
		}
	}
	var voted [FrameHeaderSize]byte
	for byteIdx := 0; byteIdx < FrameHeaderSize; byteIdx++ {
		var b byte
		for bit := 0; bit < 8; bit++ {
			ones := 0
			for i := 0; i < HeaderCopies; i++ {
				if (copies[i][byteIdx]>>uint(bit))&1 == 1 {
					ones++
				}
			}
			if ones >= 2 {
				b |= 1 << uint(bit)
			}
		}
		voted[byteIdx] = b
	}
	h, err := UnmarshalFrameHeader(voted[:])
	if err != nil || h.Version < 2 {
		return FrameHeader{}, errors.New("cabeçalho V2 não encontrado")
	}
	return h, nil
}

func decodeVotedHeader(copies [HeaderCopies][FrameHeaderSize]byte, requireHidden bool) (FrameHeader, error) {
	for i := 0; i < HeaderCopies; i++ {
		if h, err := UnmarshalFrameHeader(copies[i][:]); err == nil && h.Version >= 2 {
			if !requireHidden || (h.Flags&FlagHiddenHeader != 0) {
				return h, nil
			}
		}
	}
	var voted [FrameHeaderSize]byte
	for byteIdx := 0; byteIdx < FrameHeaderSize; byteIdx++ {
		var b byte
		for bit := 0; bit < 8; bit++ {
			ones := 0
			for i := 0; i < HeaderCopies; i++ {
				if (copies[i][byteIdx]>>uint(bit))&1 == 1 {
					ones++
				}
			}
			if ones >= 2 {
				b |= 1 << uint(bit)
			}
		}
		voted[byteIdx] = b
	}
	h, err := UnmarshalFrameHeader(voted[:])
	if err != nil || h.Version < 2 || (requireHidden && h.Flags&FlagHiddenHeader == 0) {
		return FrameHeader{}, errors.New("cabeçalho V2 não encontrado")
	}
	return h, nil
}

// Fixed non-secret whitening mask for hidden headers so zero bytes (0x00) never create repeating visual stripes!
var hiddenHeaderMask = [FrameHeaderSize]byte{
	0xA5, 0x3C, 0x91, 0x6E, 0xD2, 0x4B, 0x87, 0x1F,
	0xE4, 0x59, 0x2A, 0xC3, 0x76, 0xB8, 0x0D, 0xF1,
	0x4E, 0x9B, 0x62, 0xD7, 0x18, 0xA3, 0x55, 0xCC,
	0x39, 0x8E, 0xF4, 0x21, 0xB6, 0x0A, 0x7D, 0xE9,
}

func scrambleHiddenHeader(hb [FrameHeaderSize]byte) [FrameHeaderSize]byte {
	var out [FrameHeaderSize]byte
	for i := 0; i < FrameHeaderSize; i++ {
		out[i] = hb[i] ^ hiddenHeaderMask[i]
	}
	return out
}

// drawHeaderHiddenChroma embeds 3 redundant copies of the XOR-whitened 32-byte header into adjacent 16x16 chroma pairs
// (32x16 px = 2 bits: 1 bit U + 1 bit V) using Manchester differential ±18 around the local watercolor chroma mean!
// Result: 100% seamless dreamy watercolor edge-to-edge with ZERO neon green bar and ZERO B&W bar!
func (f *YUVFrame) drawHeaderHiddenChroma(hdr FrameHeader) {
	hb := scrambleHiddenHeader(hdr.Marshal())
	pairCols := (f.Width - 2*FrameMarginV2) / 32
	if pairCols <= 0 {
		return
	}
	U := f.UPlane()
	V := f.VPlane()
	uvStride := f.Width / 2
	cMarginX := FrameMarginV2 / 2
	cTopY := HeaderTopV2 / 2
	const amp = 18

	pairIdx := 0
	for copyIdx := 0; copyIdx < HeaderCopies; copyIdx++ {
		for _, b := range hb {
			for pair := 0; pair < 4; pair++ {
				b0 := (b >> uint(7-pair*2)) & 1
				b1 := (b >> uint(6-pair*2)) & 1
				r := pairIdx / pairCols
				c := pairIdx % pairCols
				cy := cTopY + r*8
				cx := cMarginX + c*16 // 16 chroma px = 32 luma px (left 8x8 + right 8x8)
				sumU, sumV := 0, 0
				for dy := 0; dy < 8; dy++ {
					off := (cy+dy)*uvStride + cx
					for dx := 0; dx < 16; dx++ {
						sumU += int(U[off+dx])
						sumV += int(V[off+dx])
					}
				}
				avgU := sumU / 128
				avgV := sumV / 128
				if avgU < 38 {
					avgU = 38
				} else if avgU > 218 {
					avgU = 218
				}
				if avgV < 38 {
					avgV = 38
				} else if avgV > 218 {
					avgV = 218
				}
				dUL, dUR := -amp, amp
				if b0 == 1 {
					dUL, dUR = amp, -amp
				}
				dVL, dVR := -amp, amp
				if b1 == 1 {
					dVL, dVR = amp, -amp
				}
				uL, uR := byte(avgU+dUL), byte(avgU+dUR)
				vL, vR := byte(avgV+dVL), byte(avgV+dVR)
				for dy := 0; dy < 8; dy++ {
					off := (cy+dy)*uvStride + cx
					for dx := 0; dx < 8; dx++ {
						U[off+dx] = uL
						U[off+8+dx] = uR
						V[off+dx] = vL
						V[off+8+dx] = vR
					}
				}
				pairIdx++
			}
		}
	}
	// Seamlessly fill any trailing unused pairs on the last header row so no notch/step appears!
	rem := pairIdx % pairCols
	if rem != 0 {
		st := hdr.ContainerID ^ (uint64(hdr.FrameIndex+1) * 0x9E3779B97F4A7C15) ^ 0x51A7C3D9
		for rem < pairCols {
			r := pairIdx / pairCols
			c := pairIdx % pairCols
			cy := cTopY + r*8
			cx := cMarginX + c*16
			sumU, sumV := 0, 0
			for dy := 0; dy < 8; dy++ {
				off := (cy+dy)*uvStride + cx
				for dx := 0; dx < 16; dx++ {
					sumU += int(U[off+dx])
					sumV += int(V[off+dx])
				}
			}
			avgU, avgV := sumU/128, sumV/128
			if avgU < 38 {
				avgU = 38
			} else if avgU > 218 {
				avgU = 218
			}
			if avgV < 38 {
				avgV = 38
			} else if avgV > 218 {
				avgV = 218
			}
			st ^= st << 13
			st ^= st >> 7
			st ^= st << 17
			dUL, dUR := -amp, amp
			if st&1 == 1 {
				dUL, dUR = amp, -amp
			}
			dVL, dVR := -amp, amp
			if st&2 != 0 {
				dVL, dVR = amp, -amp
			}
			uL, uR := byte(avgU+dUL), byte(avgU+dUR)
			vL, vR := byte(avgV+dVL), byte(avgV+dVR)
			for dy := 0; dy < 8; dy++ {
				off := (cy+dy)*uvStride + cx
				for dx := 0; dx < 8; dx++ {
					U[off+dx] = uL
					U[off+8+dx] = uR
					V[off+dx] = vL
					V[off+8+dx] = vR
				}
			}
			pairIdx++
			rem++
		}
	}
}

func (f *YUVFrame) readHeaderHiddenChroma() (FrameHeader, error) {
	pairCols := (f.Width - 2*FrameMarginV2) / 32
	if pairCols <= 0 {
		return FrameHeader{}, errors.New("largura insuficiente para cabeçalho oculto croma")
	}
	U := f.UPlane()
	V := f.VPlane()
	uvStride := f.Width / 2
	cMarginX := FrameMarginV2 / 2
	cTopY := HeaderTopV2 / 2

	var copies [HeaderCopies][FrameHeaderSize]byte
	pairIdx := 0
	for copyIdx := 0; copyIdx < HeaderCopies; copyIdx++ {
		for byteIdx := 0; byteIdx < FrameHeaderSize; byteIdx++ {
			var b byte
			for pair := 0; pair < 4; pair++ {
				r := pairIdx / pairCols
				c := pairIdx % pairCols
				cy := cTopY + r*8
				cx := cMarginX + c*16
				avgUL := avgPlaneRect(U, uvStride, cx+2, cy+2, 4, 4)
				avgUR := avgPlaneRect(U, uvStride, cx+10, cy+2, 4, 4)
				avgVL := avgPlaneRect(V, uvStride, cx+2, cy+2, 4, 4)
				avgVR := avgPlaneRect(V, uvStride, cx+10, cy+2, 4, 4)
				if avgUL >= avgUR {
					b |= 1 << uint(7-pair*2)
				}
				if avgVL >= avgVR {
					b |= 1 << uint(6-pair*2)
				}
				pairIdx++
			}
			copies[copyIdx][byteIdx] = b ^ hiddenHeaderMask[byteIdx]
		}
	}
	return decodeVotedHeader(copies, true)
}

// drawHeaderHiddenManchester embeds 3 redundant copies of the XOR-whitened 32-byte header into seamless art luminance
// using Manchester differential 16x8 pairs (left 8x8 vs right 8x8 = ±13 around local art mean — zero B&W bar & zero stripes!).
func (f *YUVFrame) drawHeaderHiddenManchester(hdr FrameHeader) {
	hb := scrambleHiddenHeader(hdr.Marshal())
	cols := (f.Width - 2*FrameMarginV2) / 16
	if cols <= 0 {
		return
	}
	Y := f.YPlane()
	stride := f.Width
	const amp = 13
	bitIdx := 0
	for copyIdx := 0; copyIdx < HeaderCopies; copyIdx++ {
		for _, b := range hb {
			for bit := 7; bit >= 0; bit-- {
				r := bitIdx / cols
				c := bitIdx % cols
				px := FrameMarginV2 + c*16
				py := HeaderTopV2 + r*8
				sum := 0
				for dy := 0; dy < 8; dy++ {
					off := (py+dy)*stride + px
					for dx := 0; dx < 16; dx++ {
						sum += int(Y[off+dx])
					}
				}
				avg := sum / 128
				if avg < 34 {
					avg = 34
				} else if avg > 221 {
					avg = 221
				}
				dL, dR := -amp, amp
				if (b>>uint(bit))&1 == 1 {
					dL, dR = amp, -amp
				}
				vL, vR := byte(avg+dL), byte(avg+dR)
				for dy := 0; dy < 8; dy++ {
					off := (py+dy)*stride + px
					for dx := 0; dx < 8; dx++ {
						Y[off+dx] = vL
						Y[off+8+dx] = vR
					}
				}
				bitIdx++
			}
		}
	}
	// Seamlessly fill any trailing unused cells on the last header row so no notch/step appears!
	rem := bitIdx % cols
	if rem != 0 {
		st := hdr.ContainerID ^ (uint64(hdr.FrameIndex+1) * 0x9E3779B97F4A7C15) ^ 0x73B9E105
		for rem < cols {
			r := bitIdx / cols
			c := bitIdx % cols
			px := FrameMarginV2 + c*16
			py := HeaderTopV2 + r*8
			sum := 0
			for dy := 0; dy < 8; dy++ {
				off := (py+dy)*stride + px
				for dx := 0; dx < 16; dx++ {
					sum += int(Y[off+dx])
				}
			}
			avg := sum / 128
			if avg < 34 {
				avg = 34
			} else if avg > 221 {
				avg = 221
			}
			st ^= st << 13
			st ^= st >> 7
			st ^= st << 17
			dL, dR := -amp, amp
			if st&1 == 1 {
				dL, dR = amp, -amp
			}
			vL, vR := byte(avg+dL), byte(avg+dR)
			for dy := 0; dy < 8; dy++ {
				off := (py+dy)*stride + px
				for dx := 0; dx < 8; dx++ {
					Y[off+dx] = vL
					Y[off+8+dx] = vR
				}
			}
			bitIdx++
			rem++
		}
	}
}

func (f *YUVFrame) readHeaderHiddenManchester() (FrameHeader, error) {
	cols := (f.Width - 2*FrameMarginV2) / 16
	if cols <= 0 {
		return FrameHeader{}, errors.New("largura insuficiente para cabeçalho oculto Manchester")
	}
	Y := f.YPlane()
	stride := f.Width
	var copies [HeaderCopies][FrameHeaderSize]byte
	bitIdx := 0
	for copyIdx := 0; copyIdx < HeaderCopies; copyIdx++ {
		for byteIdx := 0; byteIdx < FrameHeaderSize; byteIdx++ {
			var b byte
			for bit := 7; bit >= 0; bit-- {
				r := bitIdx / cols
				c := bitIdx % cols
				px := FrameMarginV2 + c*16
				py := HeaderTopV2 + r*8
				avgL := avgPlaneRect(Y, stride, px+2, py+2, 4, 4)
				avgR := avgPlaneRect(Y, stride, px+10, py+2, 4, 4)
				if avgL >= avgR {
					b |= 1 << uint(bit)
				}
				bitIdx++
			}
			copies[copyIdx][byteIdx] = b ^ hiddenHeaderMask[byteIdx]
		}
	}
	return decodeVotedHeader(copies, true)
}

// EncodeFrame renders payload (pre-FEC) into YUVFrame using Full-Frame V2 layout (96.2% active 4K area).
func EncodeFrame(dst *YUVFrame, prof Profile, hdr FrameHeader, payload []byte) error {
	return EncodeFrameWithHost(dst, nil, prof, hdr, payload)
}

// EncodeFrameWithHost renders payload (pre-FEC) into YUVFrame, optionally preserving an external host video's
// luminance channel (mapped to [50..206] to prevent pure black/white RGB gamut clipping) for blind chroma profiles.
func EncodeFrameWithHost(dst, host *YUVFrame, baseProf Profile, hdr FrameHeader, payload []byte) error {
	maskIntensity := MaskIntensityFromFlags(hdr.Flags)
	hideHeader := hdr.Flags&FlagHiddenHeader != 0
	// If art-stego or video-bw receives an external host video, enable blind Manchester differential luma coding
	if (baseProf.ArtMode == "subtractive-y2" || baseProf.ArtMode == "video-bw") && host != nil && host.Width == dst.Width && host.Height == dst.Height {
		hdr.Flags |= FlagHostManchester
	} else {
		hdr.Flags &^= FlagHostManchester
	}

	// Only blind chroma, subtractive-y2, host-Manchester video-bw, and geometric/3D art modes support seamless hidden headers
	canHide := CanHideHeaderArtMode(baseProf.ArtMode, hdr.Flags&FlagHostManchester != 0)
	if hideHeader && !canHide {
		hdr.Flags &^= FlagHiddenHeader
		hideHeader = false
	}

	prof := EffectiveProfile(baseProf, maskIntensity)
	if (baseProf.ArtMode == "video-bw" || baseProf.ArtMode == "subtractive-y2") && hdr.Flags&FlagHostManchester != 0 {
		switch maskIntensity {
		case MaskIntensitySubtle:
			prof.YBlockSize, prof.YLevels = 12, 2
		case MaskIntensityStrong:
			prof.YBlockSize, prof.YLevels = 8, 4
		case MaskIntensityMax:
			prof.YBlockSize, prof.YLevels = 4, 2
		default:
			prof.YBlockSize, prof.YLevels = 8, 2
		}
	}
	maxPayload := baseProf.PayloadCapacityLayoutWithFlags(dst.Width, dst.Height, true, hdr.Flags)
	if len(payload) > maxPayload {
		return fmt.Errorf("payload (%d B) excede capacidade do frame (%d B)", len(payload), maxPayload)
	}
	hdr.Version = 2
	hdr.ProfileID = baseProf.ID
	hdr.PayloadLen = uint32(len(payload))

	rawLen := baseProf.RawByteCapacityLayoutWithFlags(dst.Width, dst.Height, true, hdr.Flags)
	rs := GetRSCodec(prof.RSDataBytes)
	raw := rs.EncodeInterleaved(payload, rawLen)
	// Fill unused RS tail bytes (beyond complete 255-byte RS blocks) with balanced pseudo-random bits!
	// Without this, raw[usedRSBytes:] stays 0x00 and paints a solid green / smooth / striped bottom bar!
	if usedRS := (len(raw) / 255) * 255; usedRS < len(raw) {
		tState := hdr.ContainerID ^ (uint64(hdr.FrameIndex+1) * 0x9E3779B97F4A7C15) ^ 0xD1B54A32D192ED03
		if tState == 0 {
			tState = 0xDEADBEEFCAFEBABE
		}
		for i := usedRS; i < len(raw); i++ {
			tState ^= tState << 13
			tState ^= tState >> 7
			tState ^= tState << 17
			raw[i] = byte(tState)
		}
	}

	if IsStealthArtMode(prof.ArtMode) {
		isVisualFiller := hdr.Flags&FlagVisualFiller != 0
		if host != nil && host.Width == dst.Width && host.Height == dst.Height {
			copy(dst.Buf, host.Buf)
			if hdr.ArtTheme != ArtThemeNone {
				cb, cr := RenderThemeChromaFields(dst.Width/2, dst.Height/2, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 56, 200)
				dU, dV := dst.UPlane(), dst.VPlane()
				for i := range dU {
					dU[i] = byte((int(dU[i])*65 + int(cb[i])*35) / 100)
					dV[i] = byte((int(dV[i])*65 + int(cr[i])*35) / 100)
				}
			}
		} else if hdr.ArtTheme != ArtThemeNone {
			RenderProceduralArt(dst, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 34.0, 208.0, true)
		} else {
			renderStealthFallbackCanvas(dst, int(hdr.FrameIndex))
		}
		if !hideHeader {
			_, topYBand, _ := FrameLayoutMarginsForProfile(dst.Width, true, hdr.Flags, false)
			hdrH := topYBand - HeaderTopV2
			dst.fillYRect(FrameMarginV2, HeaderTopV2, dst.Width-2*FrameMarginV2, hdrH, 24)
			dst.fillUVRect(FrameMarginV2, HeaderTopV2, dst.Width-2*FrameMarginV2, hdrH, 128, 128)
			if !isVisualFiller {
				dst.drawCalibrationStripV2()
				dst.drawHeaderBitsV2(hdr)
			}
		}
		encodeStealthMode(dst, prof.ArtMode, hdr, raw, maskIntensity, hideHeader, isVisualFiller)
		return nil
	}

	var baseCb, baseCr []byte
	if prof.ArtMode == "video-chroma-q4" || prof.ArtMode == "video-fractal-chroma" {
		if host != nil && host.Width == dst.Width && host.Height == dst.Height {
			hY := host.YPlane()
			dY := dst.YPlane()
			for i, v := range hY {
				mapped := 50 + (int(v)-16)*156/219
				if mapped < 50 {
					mapped = 50
				} else if mapped > 206 {
					mapped = 206
				}
				dY[i] = byte(mapped)
			}
		} else {
			RenderProceduralArt(dst, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 50.0, 206.0, false)
		}
		if prof.ArtMode == "video-fractal-chroma" {
			cLo, cHi := 48, 208
			if prof.ChromaLevels == 4 {
				cLo, cHi = 56, 200
			}
			// Paint vivid dreamy theme watercolor edge-to-edge (ZERO gray borders anywhere!)
			baseCb, baseCr = RenderThemeChromaFields(dst.Width/2, dst.Height/2, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, cLo, cHi)
			copy(dst.UPlane(), baseCb)
			copy(dst.VPlane(), baseCr)
		} else if hideHeader {
			// Pre-tint chroma with matching 4-color vivid palette so the hidden header matches the frame seamlessly!
			uvW, uvH := dst.Width/2, dst.Height/2
			dU, dV := dst.UPlane(), dst.VPlane()
			cLoU, cHiU := RefCbLevels[0], RefCbLevels[1]
			cLoV, cHiV := RefCrLevels[0], RefCrLevels[1]
			if maskIntensity == MaskIntensitySubtle {
				cLoU, cHiU = 84, 172
				cLoV, cHiV = 84, 172
			} else if maskIntensity == MaskIntensityMax {
				cLoU, cHiU = 56, 200
				cLoV, cHiV = 56, 200
			}
			st := hdr.ContainerID ^ (uint64(hdr.FrameIndex+1) * 0x9E3779B97F4A7C15) ^ 0x3C6EF372
			if st == 0 {
				st = 0x123456789ABCDEF
			}
			for cy := 0; cy < uvH; cy += 8 {
				ch := 8
				if cy+ch > uvH {
					ch = uvH - cy
				}
				for cx := 0; cx < uvW; cx += 16 {
					cw := 16
					if cx+cw > uvW {
						cw = uvW - cx
					}
					st ^= st << 13
					st ^= st >> 7
					st ^= st << 17
					uBase := cLoU
					if st&1 == 1 {
						uBase = cHiU
					}
					vBase := cLoV
					if st&2 != 0 {
						vBase = cHiV
					}
					for dy := 0; dy < ch; dy++ {
						off := (cy+dy)*uvW + cx
						for dx := 0; dx < cw; dx++ {
							dU[off+dx] = uBase
							dV[off+dx] = vBase
						}
					}
				}
			}
		} else {
			fillNeutralUV(dst.UPlane(), dst.VPlane())
		}
		if !hideHeader {
			_, topYBand, _ := FrameLayoutMarginsForProfile(dst.Width, true, hdr.Flags, prof.YLevels == 0)
			hdrH := topYBand - HeaderTopV2
			dst.fillYRect(FrameMarginV2, HeaderTopV2, dst.Width-2*FrameMarginV2, hdrH, 24)
			dst.fillUVRect(FrameMarginV2, HeaderTopV2, dst.Width-2*FrameMarginV2, hdrH, 128, 128)
		}
	} else if prof.ArtMode != "" {
		if prof.ArtMode == "subtractive-y2" && host != nil && host.Width == dst.Width && host.Height == dst.Height {
			// Blind Manchester luma coding over external host video: keeps host video visible + optional artistic tint!
			hdr.Flags |= FlagHostManchester
			hY := host.YPlane()
			dY := dst.YPlane()
			for i, v := range hY {
				mapped := 44 + (int(v)-16)*168/219
				if mapped < 44 {
					mapped = 44
				} else if mapped > 212 {
					mapped = 212
				}
				dY[i] = byte(mapped)
			}
			if hdr.ArtTheme == ArtThemeNone {
				copy(dst.UPlane(), host.UPlane())
				copy(dst.VPlane(), host.VPlane())
			} else {
				// Paint dreamy theme colors over chroma (blended softly with host chroma if host is colored)
				cb, cr := RenderThemeChromaFields(dst.Width/2, dst.Height/2, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 56, 200)
				hU, hV := host.UPlane(), host.VPlane()
				dU, dV := dst.UPlane(), dst.VPlane()
				for i := range dU {
					// 65% artistic style + 35% original host video colors
					dU[i] = byte((int(cb[i])*65 + int(hU[i])*35) / 100)
					dV[i] = byte((int(cr[i])*65 + int(hV[i])*35) / 100)
				}
			}
		} else if prof.ArtMode == "video-bw" {
			if host != nil && host.Width == dst.Width && host.Height == dst.Height {
				// Pure monochrome B&W host silhouette ready for high-contrast classic static noise mask
				hY := host.YPlane()
				dY := dst.YPlane()
				for i, v := range hY {
					mapped := 70 + (int(v)-16)*116/219
					if mapped < 70 {
						mapped = 70
					} else if mapped > 186 {
						mapped = 186
					}
					dY[i] = byte(mapped)
				}
				fillNeutralUV(dst.UPlane(), dst.VPlane())
			} else {
				dst.ClearNeutral()
			}
		} else if prof.ArtMode == "subtractive-y2" {
			yLo, yHi := 44.0, 212.0
			if prof.YLevels == 4 {
				yLo, yHi = 48.0, 208.0
			}
			RenderProceduralArt(dst, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, yLo, yHi, true)
		} else if prof.ArtMode == "art-fullres" {
			RenderProceduralArt(dst, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 56.0, 200.0, true)
		} else if prof.ArtMode == "art-fractal3d" {
			RenderProceduralArt(dst, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 68.0, 188.0, true)
			if host != nil && host.Width == dst.Width && host.Height == dst.Height {
				hU, hV := host.UPlane(), host.VPlane()
				dU, dV := dst.UPlane(), dst.VPlane()
				if hdr.ArtTheme == ArtThemeNone {
					copy(dU, hU)
					copy(dV, hV)
				} else {
					for i := range dU {
						dU[i] = byte((int(dU[i])*55 + int(hU[i])*45) / 100)
						dV[i] = byte((int(dV[i])*55 + int(hV[i])*45) / 100)
					}
				}
			}
		} else if prof.ArtMode == "art-fractal-angles" {
			if host != nil && host.Width == dst.Width && host.Height == dst.Height {
				// Blind 4-angle engraving directly over external host video luminance!
				hY := host.YPlane()
				dY := dst.YPlane()
				for i, v := range hY {
					mapped := 72 + (int(v)-16)*112/219
					if mapped < 72 {
						mapped = 72
					} else if mapped > 184 {
						mapped = 184
					}
					dY[i] = byte(mapped)
				}
				if hdr.ArtTheme == ArtThemeNone {
					copy(dst.UPlane(), host.UPlane())
					copy(dst.VPlane(), host.VPlane())
				} else {
					cb, cr := RenderThemeChromaFields(dst.Width/2, dst.Height/2, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 48, 208)
					hU, hV := host.UPlane(), host.VPlane()
					dU, dV := dst.UPlane(), dst.VPlane()
					for i := range dU {
						dU[i] = byte((int(cb[i])*60 + int(hU[i])*40) / 100)
						dV[i] = byte((int(cr[i])*60 + int(hV[i])*40) / 100)
					}
				}
			} else {
				RenderProceduralArt(dst, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 72.0, 184.0, true)
			}
		} else if IsGeometricArtMode(prof.ArtMode) {
			if hideHeader {
				// Textured 16x8 luminance base matching the geometric tiles' contrast range so hidden header blends in!
				dY := dst.YPlane()
				st := hdr.ContainerID ^ (uint64(hdr.FrameIndex+1) * 0x9E3779B97F4A7C15) ^ 0x6A09E667
				if st == 0 {
					st = 0xCAFEBABE12345678
				}
				lvls := [4]byte{76, 108, 148, 180}
				for py := 0; py < dst.Height; py += 8 {
					bh := 8
					if py+bh > dst.Height {
						bh = dst.Height - py
					}
					for px := 0; px < dst.Width; px += 16 {
						bw := 16
						if px+bw > dst.Width {
							bw = dst.Width - px
						}
						st ^= st << 13
						st ^= st >> 7
						st ^= st << 17
						v := lvls[st&3]
						for dy := 0; dy < bh; dy++ {
							row := dY[(py+dy)*dst.Width+px : (py+dy)*dst.Width+px+bw]
							for dx := 0; dx < bw; dx++ {
								row[dx] = v
							}
						}
					}
				}
			} else {
				dst.ClearNeutral()
			}
			if host != nil && host.Width == dst.Width && host.Height == dst.Height && hdr.ArtTheme == ArtThemeNone {
				copy(dst.UPlane(), host.UPlane())
				copy(dst.VPlane(), host.VPlane())
			} else {
				cb, cr := RenderThemeChromaFields(dst.Width/2, dst.Height/2, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 48, 208)
				copy(dst.UPlane(), cb)
				copy(dst.VPlane(), cr)
				if host != nil && host.Width == dst.Width && host.Height == dst.Height {
					hU, hV := host.UPlane(), host.VPlane()
					dU, dV := dst.UPlane(), dst.VPlane()
					for i := range dU {
						dU[i] = byte((int(dU[i])*55 + int(hU[i])*45) / 100)
						dV[i] = byte((int(dV[i])*55 + int(hV[i])*45) / 100)
					}
				}
			}
		} else {
			dst.ClearNeutral()
			if host != nil && host.Width == dst.Width && host.Height == dst.Height && hdr.ArtTheme == ArtThemeNone {
				copy(dst.UPlane(), host.UPlane())
				copy(dst.VPlane(), host.VPlane())
			} else {
				RenderProceduralArt(dst, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 128.0, 128.0, true)
				if host != nil && host.Width == dst.Width && host.Height == dst.Height {
					// Blend host video colors & silhouettes into chroma plane (100% blind since art-chroma decodes only Y!)
					hU, hV := host.UPlane(), host.VPlane()
					dU, dV := dst.UPlane(), dst.VPlane()
					for i := range dU {
						dU[i] = byte((int(dU[i])*45 + int(hU[i])*55) / 100)
						dV[i] = byte((int(dV[i])*45 + int(hV[i])*55) / 100)
					}
				}
			}
		}
		if !hideHeader {
			// Keep ultra-slim V2 header band dark & clean so calibration + 3x CRC32 header are indestructible!
			_, topYBand, _ := FrameLayoutMarginsForProfile(dst.Width, true, hdr.Flags, prof.YLevels == 0)
			hdrH := topYBand - HeaderTopV2
			dst.fillYRect(FrameMarginV2, HeaderTopV2, dst.Width-2*FrameMarginV2, hdrH, 24)
			dst.fillUVRect(FrameMarginV2, HeaderTopV2, dst.Width-2*FrameMarginV2, hdrH, 128, 128)
		}
	} else {
		dst.ClearNeutral()
	}

	isVisualFiller := hdr.Flags&FlagVisualFiller != 0
	if !isVisualFiller {
		if hideHeader {
			if prof.ArtMode == "video-fractal-chroma" || prof.ArtMode == "video-chroma-q4" {
				dst.drawHeaderHiddenChroma(hdr)
			} else {
				dst.drawHeaderHiddenManchester(hdr)
			}
		} else {
			// Ultra-compact V2 calibration strip + 8x8 DCT-aligned 3x CRC32 header (no QR corners, no bottom bar!)
			dst.drawCalibrationStripV2()
			dst.drawHeaderBitsV2(hdr)
		}
	}

	// Render optical data grid (Full-Frame V2)
	mx, topY, botPad := FrameLayoutMarginsForProfile(dst.Width, true, hdr.Flags, prof.YLevels == 0)
	// On visual filler frames, extend texture edge-to-edge across the top header band & margins too!
	if isVisualFiller && hideHeader && prof.ArtMode != "" {
		mx, topY, botPad = 0, 0, 0
	}
	dw := dst.Width - 2*mx
	dh := dst.Height - topY - botPad
	yCols := dw / prof.YBlockSize
	yRows := dh / prof.YBlockSize
	bs := prof.YBlockSize
	Y := dst.YPlane()
	stride := dst.Width

	bitPos := 0
	padState := hdr.ContainerID ^ (uint64(hdr.FrameIndex+1) * 0x9E3779B97F4A7C15) ^ 0xA5A55A5A5A5A5A5A
	if padState == 0 {
		padState = 0xCAFEBABE12345678
	}
	nextBit := func() byte {
		byteIdx := bitPos >> 3
		if byteIdx >= len(raw) {
			bitPos++
			// Balanced pseudo-random filler for unused bottom cells beyond RS blocks (prevents jarring cutoff line!)
			padState ^= padState << 13
			padState ^= padState >> 7
			padState ^= padState << 17
			return byte(padState & 1)
		}
		shift := uint(7 - (bitPos & 7))
		b := (raw[byteIdx] >> shift) & 1
		bitPos++
		return b
	}

	if prof.YLevels == 0 {
		// Luma Y is reserved exclusively for host video / visual carrier; all data bits live in Chroma!
	} else if (prof.ArtMode == "subtractive-y2" || prof.ArtMode == "video-bw") && hdr.Flags&FlagHostManchester != 0 {
		// Blind Manchester differential luma over external host video (2 adjacent bs*bs blocks = 1 pair):
		// averages the host video's brightness across each pair and applies (+d, -d) so decoding is 100% blind!
		pairCols := yCols / 2
		area2 := 2 * bs * bs
		isBWStatic := prof.ArtMode == "video-bw"
		if prof.YLevels == 4 {
			deltas := [4]int{-24, -8, 8, 24}
			loA, hiA := 42, 210
			if isBWStatic {
				deltas = [4]int{-57, -19, 19, 57}
				loA, hiA = 74, 178
			}
			for r := 0; r < yRows; r++ {
				py := topY + r*bs
				for c := 0; c < pairCols; c++ {
					px := mx + c*2*bs
					sum := 0
					for dy := 0; dy < bs; dy++ {
						row := Y[(py+dy)*stride+px : (py+dy)*stride+px+2*bs]
						for dx := 0; dx < 2*bs; dx++ {
							sum += int(row[dx])
						}
					}
					avg := sum / area2
					if avg < loA {
						avg = loA
					} else if avg > hiA {
						avg = hiA
					}
					bits := (nextBit() << 1) | nextBit()
					d := deltas[gray2ToLevel[bits]]
					vL, vR := byte(avg+d), byte(avg-d)
					for dy := 0; dy < bs; dy++ {
						row := Y[(py+dy)*stride+px : (py+dy)*stride+px+2*bs]
						for dx := 0; dx < bs; dx++ {
							row[dx] = vL
							row[bs+dx] = vR
						}
					}
				}
			}
		} else {
			amp := 13
			loA, hiA := 36, 216
			if isBWStatic {
				// Bold high-contrast classic B&W TV static noise mask over the host video silhouette!
				amp = 52
				if maskIntensity == MaskIntensitySubtle {
					amp = 40
				} else if maskIntensity == MaskIntensityMax {
					amp = 56
				}
				loA, hiA = 74, 180
			} else if maskIntensity == MaskIntensitySubtle {
				amp = 10
			} else if maskIntensity == MaskIntensityMax {
				amp = 18
			}
			for r := 0; r < yRows; r++ {
				py := topY + r*bs
				for c := 0; c < pairCols; c++ {
					px := mx + c*2*bs
					sum := 0
					for dy := 0; dy < bs; dy++ {
						row := Y[(py+dy)*stride+px : (py+dy)*stride+px+2*bs]
						for dx := 0; dx < 2*bs; dx++ {
							sum += int(row[dx])
						}
					}
					avg := sum / area2
					if avg < loA {
						avg = loA
					} else if avg > hiA {
						avg = hiA
					}
					d := -amp
					if nextBit() == 1 {
						d = amp
					}
					vL, vR := byte(avg+d), byte(avg-d)
					for dy := 0; dy < bs; dy++ {
						row := Y[(py+dy)*stride+px : (py+dy)*stride+px+2*bs]
						for dx := 0; dx < bs; dx++ {
							row[dx] = vL
							row[bs+dx] = vR
						}
					}
				}
			}
		}
		// Seamless cosmetic border fill for blind host-Manchester modes (subtractive-y2 / video-bw)
		if hideHeader && !isVisualFiller {
			dataRight := mx + pairCols*2*bs
			dataBot := topY + yRows*bs
			bAmp := 13
			if isBWStatic {
				bAmp = 52
				if maskIntensity == MaskIntensitySubtle {
					bAmp = 40
				} else if maskIntensity == MaskIntensityMax {
					bAmp = 56
				}
			} else if maskIntensity == MaskIntensitySubtle {
				bAmp = 10
			} else if maskIntensity == MaskIntensityMax {
				bAmp = 18
			}
			for py := 0; py < dst.Height; py += bs {
				bh := bs
				if py+bh > dst.Height {
					bh = dst.Height - py
				}
				for px := 0; px < dst.Width; px += 2 * bs {
					bw := 2 * bs
					if px+bw > dst.Width {
						bw = dst.Width - px
					}
					if px >= mx && px+bw <= dataRight && py >= topY && py+bh <= dataBot {
						continue
					}
					if px >= FrameMarginV2 && px+bw <= dst.Width-FrameMarginV2 && py >= HeaderTopV2 && py+bh <= topY {
						continue
					}
					sum, cnt := 0, 0
					for dy := 0; dy < bh; dy++ {
						row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bw]
						for dx := 0; dx < bw; dx++ {
							sum += int(row[dx])
							cnt++
						}
					}
					avg := 128
					if cnt > 0 {
						avg = sum / cnt
					}
					if avg < 72 {
						avg = 72
					} else if avg > 184 {
						avg = 184
					}
					padState ^= padState << 13
					padState ^= padState >> 7
					padState ^= padState << 17
					d := -bAmp
					if padState&1 == 1 {
						d = bAmp
					}
					vL, vR := byte(avg+d), byte(avg-d)
					for dy := 0; dy < bh; dy++ {
						yy := py + dy
						inHdrY := yy >= HeaderTopV2 && yy < topY
						inDatY := yy >= topY && yy < dataBot
						row := Y[yy*stride:]
						for dx := 0; dx < bw; dx++ {
							xx := px + dx
							if (inHdrY && xx >= FrameMarginV2 && xx < dst.Width-FrameMarginV2) || (inDatY && xx >= mx && xx < dataRight) {
								continue
							}
							if dx < bs {
								row[xx] = vL
							} else {
								row[xx] = vR
							}
						}
					}
				}
			}
		}
	} else if prof.ArtMode == "subtractive-y2" && prof.YLevels == 4 {
		// Strong intensity (2x data/frame): 4-level Gray-coded subtractive carrier (-27, -9, +9, +27)
		deltas := [4]int{-27, -9, 9, 27}
		for r := 0; r < yRows; r++ {
			py := topY + r*bs
			for c := 0; c < yCols; c++ {
				px := mx + c*bs
				bits := (nextBit() << 1) | nextBit()
				delta := deltas[gray2ToLevel[bits]]
				for dy := 0; dy < bs; dy++ {
					row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bs]
					for dx := 0; dx < bs; dx++ {
						v := int(row[dx]) + delta
						if v < 16 {
							v = 16
						} else if v > 235 {
							v = 235
						}
						row[dx] = byte(v)
					}
				}
			}
		}
		if hideHeader && !isVisualFiller {
			dataRight := mx + yCols*bs
			dataBot := topY + yRows*bs
			for py := 0; py < dst.Height; py += bs {
				bh := bs
				if py+bh > dst.Height {
					bh = dst.Height - py
				}
				for px := 0; px < dst.Width; px += bs {
					bw := bs
					if px+bw > dst.Width {
						bw = dst.Width - px
					}
					if px >= mx && px+bw <= dataRight && py >= topY && py+bh <= dataBot {
						continue
					}
					if px >= FrameMarginV2 && px+bw <= dst.Width-FrameMarginV2 && py >= HeaderTopV2 && py+bh <= topY {
						continue
					}
					padState ^= padState << 13
					padState ^= padState >> 7
					padState ^= padState << 17
					delta := deltas[padState&3]
					for dy := 0; dy < bh; dy++ {
						yy := py + dy
						inHdrY := yy >= HeaderTopV2 && yy < topY
						inDatY := yy >= topY && yy < dataBot
						row := Y[yy*stride:]
						for dx := 0; dx < bw; dx++ {
							xx := px + dx
							if (inHdrY && xx >= FrameMarginV2 && xx < dst.Width-FrameMarginV2) || (inDatY && xx >= mx && xx < dataRight) {
								continue
							}
							v := int(row[xx]) + delta
							if v < 16 {
								v = 16
							} else if v > 235 {
								v = 235
							}
							row[xx] = byte(v)
						}
					}
				}
			}
		}
	} else if prof.ArtMode == "subtractive-y2" {
		amp := 12 // Balanced default (±12 on 8x8)
		if maskIntensity == MaskIntensitySubtle {
			amp = 9 // Subtle (±9 on 16x16, ~30 dB PSNR art clarity)
		} else if maskIntensity == MaskIntensityMax {
			amp = 18 // Max (±18 on 4x4, 4x data/frame)
		}
		for r := 0; r < yRows; r++ {
			py := topY + r*bs
			for c := 0; c < yCols; c++ {
				px := mx + c*bs
				delta := -amp
				if nextBit() == 1 {
					delta = amp
				}
				for dy := 0; dy < bs; dy++ {
					row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bs]
					for dx := 0; dx < bs; dx++ {
						v := int(row[dx]) + delta
						if v < 16 {
							v = 16
						} else if v > 235 {
							v = 235
						}
						row[dx] = byte(v)
					}
				}
			}
		}
		if hideHeader && !isVisualFiller {
			dataRight := mx + yCols*bs
			dataBot := topY + yRows*bs
			for py := 0; py < dst.Height; py += bs {
				bh := bs
				if py+bh > dst.Height {
					bh = dst.Height - py
				}
				for px := 0; px < dst.Width; px += bs {
					bw := bs
					if px+bw > dst.Width {
						bw = dst.Width - px
					}
					if px >= mx && px+bw <= dataRight && py >= topY && py+bh <= dataBot {
						continue
					}
					if px >= FrameMarginV2 && px+bw <= dst.Width-FrameMarginV2 && py >= HeaderTopV2 && py+bh <= topY {
						continue
					}
					padState ^= padState << 13
					padState ^= padState >> 7
					padState ^= padState << 17
					delta := -amp
					if padState&1 == 1 {
						delta = amp
					}
					for dy := 0; dy < bh; dy++ {
						yy := py + dy
						inHdrY := yy >= HeaderTopV2 && yy < topY
						inDatY := yy >= topY && yy < dataBot
						row := Y[yy*stride:]
						for dx := 0; dx < bw; dx++ {
							xx := px + dx
							if (inHdrY && xx >= FrameMarginV2 && xx < dst.Width-FrameMarginV2) || (inDatY && xx >= mx && xx < dataRight) {
								continue
							}
							v := int(row[xx]) + delta
							if v < 16 {
								v = 16
							} else if v > 235 {
								v = 235
							}
							row[xx] = byte(v)
						}
					}
				}
			}
		}
	} else if prof.ArtMode == "art-fullres" {
		// Full-Res Crisp 2-bit Subtractive: adds {-40,-14,+14,+40} per pixel (or {-28,-10,+10,+28} in Subtle)
		// WITHOUT flattening 8x8 blocks — preserving razor-sharp DeepDream / Mandelbrot spirals!
		deltas := [4]int{-40, -14, 14, 40}
		if maskIntensity == MaskIntensitySubtle {
			deltas = [4]int{-28, -10, 10, 28}
		}
		for r := 0; r < yRows; r++ {
			py := topY + r*bs
			for c := 0; c < yCols; c++ {
				px := mx + c*bs
				bits := (nextBit() << 1) | nextBit()
				delta := deltas[gray2ToLevel[bits]]
				for dy := 0; dy < bs; dy++ {
					row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bs]
					for dx := 0; dx < bs; dx++ {
						v := int(row[dx]) + delta
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
		// Seamless edge-to-edge cosmetic texture on outer safety borders (strictly outside data & header!)
		if hideHeader && !isVisualFiller {
			dataRight := mx + yCols*bs
			dataBot := topY + yRows*bs
			for py := 0; py < dst.Height; py += bs {
				bh := bs
				if py+bh > dst.Height {
					bh = dst.Height - py
				}
				for px := 0; px < dst.Width; px += bs {
					bw := bs
					if px+bw > dst.Width {
						bw = dst.Width - px
					}
					padState ^= padState << 13
					padState ^= padState >> 7
					padState ^= padState << 17
					delta := deltas[padState&3]
					for dy := 0; dy < bh; dy++ {
						yy := py + dy
						inHdrY := yy >= HeaderTopV2 && yy < topY
						inDatY := yy >= topY && yy < dataBot
						row := Y[yy*stride:]
						for dx := 0; dx < bw; dx++ {
							xx := px + dx
							if (inHdrY && xx >= FrameMarginV2 && xx < dst.Width-FrameMarginV2) || (inDatY && xx >= mx && xx < dataRight) {
								continue
							}
							v := int(row[xx]) + delta
							if v < 16 {
								v = 16
							} else if v > 240 {
								v = 240
							}
							row[xx] = byte(v)
						}
					}
				}
			}
		}
	} else if prof.ArtMode == "art-fractal3d" {
		// 3D Fractal Relief: block-flat 4-level Gray-coded subtractive (±48, ±16) over 3D art luminance [68..188]
		area := bs * bs
		for r := 0; r < yRows; r++ {
			py := topY + r*bs
			for c := 0; c < yCols; c++ {
				px := mx + c*bs
				sum := 0
				for dy := 0; dy < bs; dy++ {
					row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bs]
					for dx := 0; dx < bs; dx++ {
						sum += int(row[dx])
					}
				}
				base := sum / area
				bits := (nextBit() << 1) | nextBit()
				v := base + patFractal3D[gray2ToLevel[bits]]
				if v < 16 {
					v = 16
				} else if v > 240 {
					v = 240
				}
				bv := byte(v)
				for dy := 0; dy < bs; dy++ {
					row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bs]
					for dx := 0; dx < bs; dx++ {
						row[dx] = bv
					}
				}
			}
		}
		if hideHeader && !isVisualFiller {
			dataRight := mx + yCols*bs
			dataBot := topY + yRows*bs
			for py := 0; py < dst.Height; py += bs {
				bh := bs
				if py+bh > dst.Height {
					bh = dst.Height - py
				}
				for px := 0; px < dst.Width; px += bs {
					bw := bs
					if px+bw > dst.Width {
						bw = dst.Width - px
					}
					if px >= mx && px+bw <= dataRight && py >= topY && py+bh <= dataBot {
						continue
					}
					if px >= FrameMarginV2 && px+bw <= dst.Width-FrameMarginV2 && py >= HeaderTopV2 && py+bh <= topY {
						continue
					}
					sum, cnt := 0, 0
					for dy := 0; dy < bh; dy++ {
						row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bw]
						for dx := 0; dx < bw; dx++ {
							sum += int(row[dx])
							cnt++
						}
					}
					base := 128
					if cnt > 0 {
						base = sum / cnt
					}
					padState ^= padState << 13
					padState ^= padState >> 7
					padState ^= padState << 17
					v := base + patFractal3D[padState&3]
					if v < 16 {
						v = 16
					} else if v > 240 {
						v = 240
					}
					bv := byte(v)
					for dy := 0; dy < bh; dy++ {
						yy := py + dy
						inHdrY := yy >= HeaderTopV2 && yy < topY
						inDatY := yy >= topY && yy < dataBot
						row := Y[yy*stride:]
						for dx := 0; dx < bw; dx++ {
							xx := px + dx
							if (inHdrY && xx >= FrameMarginV2 && xx < dst.Width-FrameMarginV2) || (inDatY && xx >= mx && xx < dataRight) {
								continue
							}
							row[xx] = bv
						}
					}
				}
			}
		}
	} else if IsGeometricArtMode(prof.ArtMode) {
		bitsPerCell := 3
		if prof.YLevels == 4 {
			bitsPerCell = 2
		}
		for r := 0; r < yRows; r++ {
			py := topY + r*bs
			for c := 0; c < yCols; c++ {
				px := mx + c*bs
				sym := 0
				for b := 0; b < bitsPerCell; b++ {
					sym = (sym << 1) | int(nextBit())
				}
				encodeGeometricCell(prof.ArtMode, Y, stride, px, py, bs, sym)
			}
		}
		// Seamless edge-to-edge cosmetic geometric tiles across top/bottom/side safety borders (zero black bars!)
		if hideHeader && !isVisualFiller {
			dataRight := mx + yCols*bs
			dataBot := topY + yRows*bs
			symMask := uint64((1 << bitsPerCell) - 1)
			tmpTile := make([]byte, bs*bs)
			for py := 0; py < dst.Height; py += bs {
				bh := bs
				if py+bh > dst.Height {
					bh = dst.Height - py
				}
				for px := 0; px < dst.Width; px += bs {
					bw := bs
					if px+bw > dst.Width {
						bw = dst.Width - px
					}
					// Skip tile quickly if 100% inside data grid or header band
					if px >= mx && px+bw <= dataRight && py >= topY && py+bh <= dataBot {
						continue
					}
					if px >= FrameMarginV2 && px+bw <= dst.Width-FrameMarginV2 && py >= HeaderTopV2 && py+bh <= topY {
						continue
					}
					padState ^= padState << 13
					padState ^= padState >> 7
					padState ^= padState << 17
					sym := int(padState & symMask)
					// Copy local Y context into tmpTile (for blind art-fractal-angles over host video!)
					for dy := 0; dy < bs; dy++ {
						sy := py + dy
						if sy >= dst.Height {
							sy = dst.Height - 1
						}
						for dx := 0; dx < bs; dx++ {
							sx := px + dx
							if sx >= dst.Width {
								sx = dst.Width - 1
							}
							tmpTile[dy*bs+dx] = Y[sy*stride+sx]
						}
					}
					encodeGeometricCell(prof.ArtMode, tmpTile, bs, 0, 0, bs, sym)
					for dy := 0; dy < bh; dy++ {
						yy := py + dy
						inHdrY := yy >= HeaderTopV2 && yy < topY
						inDatY := yy >= topY && yy < dataBot
						row := Y[yy*stride:]
						for dx := 0; dx < bw; dx++ {
							xx := px + dx
							if (inHdrY && xx >= FrameMarginV2 && xx < dst.Width-FrameMarginV2) || (inDatY && xx >= mx && xx < dataRight) {
								continue
							}
							row[xx] = tmpTile[dy*bs+dx]
						}
					}
				}
			}
		}
	} else if prof.YLevels == 6 {
		// 6-level flat gray: 5 bits (0..31) packed across 2 adjacent cells (6x6 = 36 states)
		pairs := (yCols * yRows) / 2
		for p := 0; p < pairs; p++ {
			v := int((nextBit() << 4) | (nextBit() << 3) | (nextBit() << 2) | (nextBit() << 1) | nextBit())
			vals := [2]byte{RefY6Levels[v/6], RefY6Levels[v%6]}
			for k := 0; k < 2; k++ {
				cell := p*2 + k
				r := cell / yCols
				c := cell % yCols
				py := topY + r*bs
				px := mx + c*bs
				yVal := vals[k]
				for dy := 0; dy < bs; dy++ {
					row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bs]
					for dx := 0; dx < bs; dx++ {
						row[dx] = yVal
					}
				}
			}
		}
	} else {
		for r := 0; r < yRows; r++ {
			py := topY + r*bs
			for c := 0; c < yCols; c++ {
				px := mx + c*bs
				var yVal byte
				switch prof.YLevels {
				case 8:
					bits := (nextBit() << 2) | (nextBit() << 1) | nextBit()
					yVal = RefY8Levels[gray3ToLevel[bits]]
				case 4:
					bits := (nextBit() << 1) | nextBit()
					yVal = RefYLevels[gray2ToLevel[bits]]
				default:
					if nextBit() == 1 {
						yVal = RefYLevels[3]
					} else {
						yVal = RefYLevels[0]
					}
				}
				for dy := 0; dy < bs; dy++ {
					row := Y[(py+dy)*stride+px : (py+dy)*stride+px+bs]
					for dx := 0; dx < bs; dx++ {
						row[dx] = yVal
					}
				}
			}
		}
	}

	if prof.UseChroma {
		cCols := dw / prof.ChromaBlockY
		cRows := dh / prof.ChromaBlockY
		cbs := prof.ChromaBlockY / 2 // size in 4:2:0 chroma plane
		U := dst.UPlane()
		V := dst.VPlane()
		uvStride := dst.Width / 2
		cMarginX := mx / 2
		cTopY := topY / 2

		if prof.ArtMode == "video-fractal-chroma" {
			if baseCb == nil {
				cLo, cHi := 48, 208
				if prof.ChromaLevels == 4 {
					cLo, cHi = 56, 200
				}
				baseCb, baseCr = RenderThemeChromaFields(uvStride, dst.Height/2, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, cLo, cHi)
			}
			if prof.ChromaLevels == 4 {
				// Strong intensity (2x data/frame): 4-level Gray-coded subtractive chroma (-33, -11, +11, +33)
				cDeltas := [4]int{-33, -11, 11, 33}
				for r := 0; r < cRows; r++ {
					cy := cTopY + r*cbs
					for c := 0; c < cCols; c++ {
						cx := cMarginX + c*cbs
						uBits := (nextBit() << 1) | nextBit()
						vBits := (nextBit() << 1) | nextBit()
						dCb := cDeltas[gray2ToLevel[uBits]]
						dCr := cDeltas[gray2ToLevel[vBits]]
						for dy := 0; dy < cbs; dy++ {
							off := (cy+dy)*uvStride + cx
							for dx := 0; dx < cbs; dx++ {
								uVal := int(baseCb[off+dx]) + dCb
								if uVal < 20 {
									uVal = 20
								} else if uVal > 236 {
									uVal = 236
								}
								vVal := int(baseCr[off+dx]) + dCr
								if vVal < 20 {
									vVal = 20
								} else if vVal > 236 {
									vVal = 236
								}
								U[off+dx] = byte(uVal)
								V[off+dx] = byte(vVal)
							}
						}
					}
				}
			} else {
				amp := 16 // Silky watercolor default (±16 on 16x16 = 8x8 chroma DCT — dreamy & rock-solid in VP9!)
				if maskIntensity == MaskIntensitySubtle {
					amp = 12 // Ultra-clean subtle (±12 on 24x24)
				} else if maskIntensity == MaskIntensityMax {
					amp = 22 // Max (±22 on 8x8 = 4x4 U/V)
				}
				for r := 0; r < cRows; r++ {
					cy := cTopY + r*cbs
					for c := 0; c < cCols; c++ {
						cx := cMarginX + c*cbs
						dCb := -amp
						if nextBit() == 1 {
							dCb = amp
						}
						dCr := -amp
						if nextBit() == 1 {
							dCr = amp
						}
						for dy := 0; dy < cbs; dy++ {
							off := (cy+dy)*uvStride + cx
							for dx := 0; dx < cbs; dx++ {
								uVal := int(baseCb[off+dx]) + dCb
								if uVal < 20 {
									uVal = 20
								} else if uVal > 236 {
									uVal = 236
								}
								vVal := int(baseCr[off+dx]) + dCr
								if vVal < 20 {
									vVal = 20
								} else if vVal > 236 {
									vVal = 236
								}
								U[off+dx] = byte(uVal)
								V[off+dx] = byte(vVal)
							}
						}
					}
				}
			}
		} else if prof.ChromaLevels == 4 {
			refC4 := [4]byte{44, 100, 156, 212}
			for r := 0; r < cRows; r++ {
				cy := cTopY + r*cbs
				for c := 0; c < cCols; c++ {
					cx := cMarginX + c*cbs
					uBits := (nextBit() << 1) | nextBit()
					vBits := (nextBit() << 1) | nextBit()
					cbVal := refC4[gray2ToLevel[uBits]]
					crVal := refC4[gray2ToLevel[vBits]]
					for dy := 0; dy < cbs; dy++ {
						off := (cy+dy)*uvStride + cx
						uRow := U[off : off+cbs]
						vRow := V[off : off+cbs]
						for dx := 0; dx < cbs; dx++ {
							uRow[dx] = cbVal
							vRow[dx] = crVal
						}
					}
				}
			}
		} else {
			cbLvls := RefCbLevels
			crLvls := RefCrLevels
			if prof.ArtMode == "video-chroma-q4" {
				if maskIntensity == MaskIntensitySubtle {
					cbLvls = [2]byte{84, 172}
					crLvls = [2]byte{84, 172}
				} else if maskIntensity == MaskIntensityMax {
					cbLvls = [2]byte{52, 204}
					crLvls = [2]byte{52, 204}
				}
			}
			for r := 0; r < cRows; r++ {
				cy := cTopY + r*cbs
				for c := 0; c < cCols; c++ {
					cx := cMarginX + c*cbs
					cbBit := nextBit()
					crBit := nextBit()
					cbVal := cbLvls[cbBit]
					crVal := crLvls[crBit]
					for dy := 0; dy < cbs; dy++ {
						off := (cy+dy)*uvStride + cx
						uRow := U[off : off+cbs]
						vRow := V[off : off+cbs]
						for dx := 0; dx < cbs; dx++ {
							uRow[dx] = cbVal
							vRow[dx] = crVal
						}
					}
				}
			}
		}

		// Seamless edge-to-edge cosmetic chroma fill on outer safety borders (eliminates top/bottom/side bars!)
		if hideHeader && !isVisualFiller && (prof.ArtMode == "video-fractal-chroma" || prof.ArtMode == "video-chroma-q4") {
			uvH := dst.Height / 2
			cHdrMargin := FrameMarginV2 / 2
			cHdrTop := HeaderTopV2 / 2
			cDataRight := cMarginX + cCols*cbs
			cDataBot := cTopY + cRows*cbs
			cDeltas4 := [4]int{-33, -11, 11, 33}
			refC4 := [4]byte{44, 100, 156, 212}
			cbLvls := RefCbLevels
			crLvls := RefCrLevels
			if prof.ArtMode == "video-chroma-q4" {
				if maskIntensity == MaskIntensitySubtle {
					cbLvls = [2]byte{84, 172}
					crLvls = [2]byte{84, 172}
				} else if maskIntensity == MaskIntensityMax {
					cbLvls = [2]byte{52, 204}
					crLvls = [2]byte{52, 204}
				}
			}
			amp := 16
			if maskIntensity == MaskIntensitySubtle {
				amp = 12
			} else if maskIntensity == MaskIntensityMax {
				amp = 22
			}
			for cy := 0; cy < uvH; cy += cbs {
				ch := cbs
				if cy+ch > uvH {
					ch = uvH - cy
				}
				for cx := 0; cx < uvStride; cx += cbs {
					cw := cbs
					if cx+cw > uvStride {
						cw = uvStride - cx
					}
					if cx >= cMarginX && cx+cw <= cDataRight && cy >= cTopY && cy+ch <= cDataBot {
						continue
					}
					if cx >= cHdrMargin && cx+cw <= uvStride-cHdrMargin && cy >= cHdrTop && cy+ch <= cTopY {
						continue
					}
					padState ^= padState << 13
					padState ^= padState >> 7
					padState ^= padState << 17
					for dy := 0; dy < ch; dy++ {
						yy := cy + dy
						inHdrY := yy >= cHdrTop && yy < cTopY
						inDatY := yy >= cTopY && yy < cDataBot
						rowOff := yy * uvStride
						for dx := 0; dx < cw; dx++ {
							xx := cx + dx
							if (inHdrY && xx >= cHdrMargin && xx < uvStride-cHdrMargin) || (inDatY && xx >= cMarginX && xx < cDataRight) {
								continue
							}
							idx := rowOff + xx
							if prof.ArtMode == "video-fractal-chroma" && baseCb != nil {
								var dCb, dCr int
								if prof.ChromaLevels == 4 {
									dCb = cDeltas4[padState&3]
									dCr = cDeltas4[(padState>>2)&3]
								} else {
									dCb = -amp
									if padState&1 == 1 {
										dCb = amp
									}
									dCr = -amp
									if padState&2 != 0 {
										dCr = amp
									}
								}
								uVal := int(baseCb[idx]) + dCb
								if uVal < 20 {
									uVal = 20
								} else if uVal > 236 {
									uVal = 236
								}
								vVal := int(baseCr[idx]) + dCr
								if vVal < 20 {
									vVal = 20
								} else if vVal > 236 {
									vVal = 236
								}
								U[idx] = byte(uVal)
								V[idx] = byte(vVal)
							} else if prof.ChromaLevels == 4 {
								U[idx] = refC4[padState&3]
								V[idx] = refC4[(padState>>2)&3]
							} else {
								U[idx] = cbLvls[padState&1]
								V[idx] = crLvls[(padState>>1)&1]
							}
						}
					}
				}
			}
		}
	}

	return nil
}

const (
	HeaderKindUnknown          uint8 = 0
	HeaderKindV2               uint8 = 1
	HeaderKindHiddenChroma     uint8 = 2
	HeaderKindHiddenManchester uint8 = 3
	HeaderKindHiddenStealth    uint8 = 4
	HeaderKindV1               uint8 = 5
)

// DecodeHint accelerates multi-frame decoding by trying the video's known header format & stealth phase first.
type DecodeHint struct {
	HeaderKind    uint8
	ProfileID     ProfileID
	ExpectedPhase int
}

// DecodeFrameResult contains the recovered payload and error correction telemetry.
type DecodeFrameResult struct {
	Header         FrameHeader
	HeaderKind     uint8
	Payload        []byte
	CorrectedBytes int
	RawByteCount   int
}

func defaultCalThresholds() calThresholds {
	return calThresholds{
		yThresh2: 129.5,
		yThresh4: [3]float64{60.0, 132.0, 201.5},
		yThresh6: [5]float64{45.1, 87.3, 129.5, 171.7, 213.9},
		yThresh8: [7]float64{39.1, 69.2, 99.4, 129.5, 159.6, 189.8, 219.9},
		cbThresh: 128.0,
		crThresh: 128.0,
	}
}

// DecodeFrame reads an optical YUV420p frame (auto-detecting V2 B&W, Hidden Chroma, Hidden Manchester, Hidden Stealth, or legacy V1),
// auto-calibrates, extracts symbols, and runs Reed-Solomon FEC.
func DecodeFrame(src *YUVFrame) (*DecodeFrameResult, error) {
	return DecodeFrameWithHint(src, DecodeHint{})
}

// DecodeFrameWithHint decodes a frame trying the hinted header format first (up to 20x faster on stealth streams!),
// transparently falling back to full auto-detection if the hint misses.
func DecodeFrameWithHint(src *YUVFrame, hint DecodeHint) (*DecodeFrameResult, error) {
	var (
		cal      calThresholds
		hdr      FrameHeader
		err      error = errors.New("uninitialized")
		layoutV2       = true
		hdrKind  uint8
	)

	// Fast-path: try the known header type first (avoids 20+ failed probes per frame on hidden/stealth videos!)
	switch hint.HeaderKind {
	case HeaderKindHiddenStealth:
		if hS, sErr := src.readHeaderHiddenStealthHint(hint.ProfileID, hint.ExpectedPhase); sErr == nil {
			hdr, err, cal, hdrKind = hS, nil, defaultCalThresholds(), HeaderKindHiddenStealth
		}
	case HeaderKindHiddenChroma:
		if hC, cErr := src.readHeaderHiddenChroma(); cErr == nil {
			hdr, err, cal, hdrKind = hC, nil, defaultCalThresholds(), HeaderKindHiddenChroma
		}
	case HeaderKindHiddenManchester:
		if hM, mErr := src.readHeaderHiddenManchester(); mErr == nil {
			hdr, err, cal, hdrKind = hM, nil, defaultCalThresholds(), HeaderKindHiddenManchester
		}
	}

	if err != nil {
		cal = src.readCalibrationV2()
		hdr, err = src.readHeaderV2(cal)
		if err == nil {
			hdrKind = HeaderKindV2
		}
	}
	if err != nil && hint.HeaderKind != HeaderKindHiddenChroma {
		// Try seamless Hidden Chroma Header (video-fractal / video-chroma with zero top bar)
		if hChroma, cErr := src.readHeaderHiddenChroma(); cErr == nil {
			hdr, err, cal, hdrKind = hChroma, nil, defaultCalThresholds(), HeaderKindHiddenChroma
		}
	}
	if err != nil && hint.HeaderKind != HeaderKindHiddenManchester {
		// Try seamless Hidden Manchester Luma Header (art-stego with zero top bar)
		if hMan, mErr := src.readHeaderHiddenManchester(); mErr == nil {
			hdr, err, cal, hdrKind = hMan, nil, defaultCalThresholds(), HeaderKindHiddenManchester
		}
	}
	if err != nil {
		// Try seamless full-screen Stealth Mask Header (VHS / Rain / 35mm Film / Stars — zero top bar!)
		if hStealth, sErr := src.readHeaderHiddenStealth(); sErr == nil {
			hdr, err, cal, hdrKind = hStealth, nil, defaultCalThresholds(), HeaderKindHiddenStealth
		}
	}
	if err != nil {
		// Automatic fallback to legacy V1 (YTC1) layout for 100% backwards compatibility!
		cal = src.readCalibration()
		hdr, err = src.readHeader(cal)
		if err != nil {
			return nil, fmt.Errorf("erro lendo cabeçalho do frame: %w", err)
		}
		layoutV2 = false
		hdrKind = HeaderKindV1
	}
	baseProf, ok := Profiles[hdr.ProfileID]
	if !ok {
		return nil, fmt.Errorf("perfil de frame desconhecido: %d", hdr.ProfileID)
	}
	maskIntensity := MaskIntensityFromFlags(hdr.Flags)
	prof := EffectiveProfile(baseProf, maskIntensity)
	if (baseProf.ArtMode == "video-bw" || baseProf.ArtMode == "subtractive-y2") && hdr.Flags&FlagHostManchester != 0 {
		switch maskIntensity {
		case MaskIntensitySubtle:
			prof.YBlockSize, prof.YLevels = 12, 2
		case MaskIntensityStrong:
			prof.YBlockSize, prof.YLevels = 8, 4
		case MaskIntensityMax:
			prof.YBlockSize, prof.YLevels = 4, 2
		default:
			prof.YBlockSize, prof.YLevels = 8, 2
		}
	}

	rawLen := baseProf.RawByteCapacityLayoutWithFlags(src.Width, src.Height, layoutV2, hdr.Flags)
	if IsStealthArtMode(prof.ArtMode) {
		raw, rawConf := decodeStealthMode(src, prof.ArtMode, hdr, layoutV2, rawLen)
		rs := GetRSCodec(prof.RSDataBytes)
		recovered, corrected, err := rs.DecodeInterleavedSoft(raw, rawConf)
		if err != nil {
			return nil, fmt.Errorf("frame %d falhou na correção FEC: %w", hdr.FrameIndex, err)
		}
		if int(hdr.PayloadLen) > len(recovered) {
			return nil, fmt.Errorf("frame %d payload_len inválido (%d > %d)", hdr.FrameIndex, hdr.PayloadLen, len(recovered))
		}
		return &DecodeFrameResult{
			Header:         hdr,
			HeaderKind:     hdrKind,
			Payload:        recovered[:hdr.PayloadLen],
			CorrectedBytes: corrected,
			RawByteCount:   rawLen,
		}, nil
	}
	raw := make([]byte, rawLen)

	mx, topY, botPad := FrameLayoutMarginsForProfile(src.Width, layoutV2, hdr.Flags, prof.YLevels == 0)
	dw := src.Width - 2*mx
	dh := src.Height - topY - botPad
	yCols := dw / prof.YBlockSize
	yRows := dh / prof.YBlockSize
	bs := prof.YBlockSize
	Y := src.YPlane()
	stride := src.Width

	// Inner-core sampling margins to avoid DCT block boundary ringing
	pad := 1
	if bs >= 8 {
		pad = 2
	}
	core := bs - 2*pad
	if core < 1 {
		pad = 0
		core = bs
	}
	coreArea := float64(core * core)

	bitPos := 0
	putBit := func(b byte) {
		byteIdx := bitPos >> 3
		if byteIdx < len(raw) && b != 0 {
			raw[byteIdx] |= 1 << uint(7-(bitPos&7))
		}
		bitPos++
	}

	if prof.YLevels == 0 {
		// Blind Chroma-only profile: ignore host video luminance Y completely!
	} else if (prof.ArtMode == "subtractive-y2" || prof.ArtMode == "video-bw") && hdr.Flags&FlagHostManchester != 0 {
		// Blind decode of Manchester differential luma over host video (NO host video needed!)
		pairCols := yCols / 2
		thresh4 := 16.0
		if prof.ArtMode == "video-bw" {
			thresh4 = 38.0 // midpoint between 19 and 57
		}
		for r := 0; r < yRows; r++ {
			py := topY + r*bs + pad
			for c := 0; c < pairCols; c++ {
				pxL := mx + c*2*bs + pad
				pxR := pxL + bs
				sumL, sumR := 0, 0
				for dy := 0; dy < core; dy++ {
					offL := (py+dy)*stride + pxL
					offR := (py+dy)*stride + pxR
					for dx := 0; dx < core; dx++ {
						sumL += int(Y[offL+dx])
						sumR += int(Y[offR+dx])
					}
				}
				if prof.YLevels == 4 {
					halfDiff := float64(sumL-sumR) / (2.0 * coreArea)
					lvl := 0
					if halfDiff >= thresh4 {
						lvl = 3
					} else if halfDiff >= 0.0 {
						lvl = 2
					} else if halfDiff >= -thresh4 {
						lvl = 1
					}
					g := levelToGray2[lvl]
					putBit((g >> 1) & 1)
					putBit(g & 1)
				} else {
					if sumL >= sumR {
						putBit(1)
					} else {
						putBit(0)
					}
				}
			}
		}
	} else if prof.ArtMode == "subtractive-y2" && prof.YLevels == 4 {
		artBuf := NewYUVFrame(src.Width, src.Height)
		RenderProceduralArt(artBuf, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 48.0, 208.0, false)
		artY := artBuf.YPlane()
		for r := 0; r < yRows; r++ {
			py := topY + r*bs + pad
			for c := 0; c < yCols; c++ {
				px := mx + c*bs + pad
				diffSum := 0
				for dy := 0; dy < core; dy++ {
					off := (py+dy)*stride + px
					for dx := 0; dx < core; dx++ {
						diffSum += int(Y[off+dx]) - int(artY[off+dx])
					}
				}
				avgDiff := float64(diffSum) / coreArea
				lvl := 0
				if avgDiff >= 18.0 {
					lvl = 3
				} else if avgDiff >= 0.0 {
					lvl = 2
				} else if avgDiff >= -18.0 {
					lvl = 1
				}
				g := levelToGray2[lvl]
				putBit((g >> 1) & 1)
				putBit(g & 1)
			}
		}
	} else if prof.ArtMode == "subtractive-y2" {
		// Regenerate exact deterministic procedural art carrier A(x,y) and subtract it out!
		artBuf := NewYUVFrame(src.Width, src.Height)
		yLo, yHi := 44.0, 212.0
		if !layoutV2 {
			yLo, yHi = 48.0, 208.0
		}
		RenderProceduralArt(artBuf, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, yLo, yHi, false)
		artY := artBuf.YPlane()
		for r := 0; r < yRows; r++ {
			py := topY + r*bs + pad
			for c := 0; c < yCols; c++ {
				px := mx + c*bs + pad
				diffSum := 0
				for dy := 0; dy < core; dy++ {
					off := (py+dy)*stride + px
					for dx := 0; dx < core; dx++ {
						diffSum += int(Y[off+dx]) - int(artY[off+dx])
					}
				}
				if diffSum >= 0 {
					putBit(1)
				} else {
					putBit(0)
				}
			}
		}
	} else if prof.ArtMode == "art-fullres" {
		artBuf := NewYUVFrame(src.Width, src.Height)
		RenderProceduralArt(artBuf, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 56.0, 200.0, false)
		artY := artBuf.YPlane()
		thresh := 27.0 // midpoint of 14 and 40
		if maskIntensity == MaskIntensitySubtle {
			thresh = 19.0 // midpoint of 10 and 28
		}
		for r := 0; r < yRows; r++ {
			py := topY + r*bs + pad
			for c := 0; c < yCols; c++ {
				px := mx + c*bs + pad
				diffSum := 0
				for dy := 0; dy < core; dy++ {
					off := (py+dy)*stride + px
					for dx := 0; dx < core; dx++ {
						diffSum += int(Y[off+dx]) - int(artY[off+dx])
					}
				}
				avgDiff := float64(diffSum) / coreArea
				lvl := 0
				if avgDiff >= thresh {
					lvl = 3
				} else if avgDiff >= 0.0 {
					lvl = 2
				} else if avgDiff >= -thresh {
					lvl = 1
				}
				g := levelToGray2[lvl]
				putBit((g >> 1) & 1)
				putBit(g & 1)
			}
		}
	} else if prof.ArtMode == "art-fractal3d" {
		artBuf := NewYUVFrame(src.Width, src.Height)
		RenderProceduralArt(artBuf, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, 68.0, 188.0, false)
		artY := artBuf.YPlane()
		area := float64(bs * bs)
		for r := 0; r < yRows; r++ {
			pyFull := topY + r*bs
			py := pyFull + pad
			for c := 0; c < yCols; c++ {
				pxFull := mx + c*bs
				px := pxFull + pad
				// Compute exact full-block art mean (matching encoder's base average)
				artSum := 0
				for dy := 0; dy < bs; dy++ {
					off := (pyFull+dy)*stride + pxFull
					for dx := 0; dx < bs; dx++ {
						artSum += int(artY[off+dx])
					}
				}
				recSum := 0
				for dy := 0; dy < core; dy++ {
					off := (py+dy)*stride + px
					for dx := 0; dx < core; dx++ {
						recSum += int(Y[off+dx])
					}
				}
				diff := float64(recSum)/coreArea - float64(artSum)/area
				lvl := 0
				if diff >= 32.0 {
					lvl = 3
				} else if diff >= 0.0 {
					lvl = 2
				} else if diff >= -32.0 {
					lvl = 1
				}
				g := levelToGray2[lvl]
				putBit((g >> 1) & 1)
				putBit(g & 1)
			}
		}
	} else if IsGeometricArtMode(prof.ArtMode) {
		bitsPerCell := 3
		if prof.YLevels == 4 {
			bitsPerCell = 2
		}
		for r := 0; r < yRows; r++ {
			py := topY + r*bs
			for c := 0; c < yCols; c++ {
				px := mx + c*bs
				sym := decodeGeometricCell(prof.ArtMode, Y, stride, px, py, bs)
				for b := bitsPerCell - 1; b >= 0; b-- {
					putBit(byte((sym >> uint(b)) & 1))
				}
			}
		}
	} else if prof.YLevels == 6 {
		pairs := (yCols * yRows) / 2
		for p := 0; p < pairs; p++ {
			var lvls [2]int
			for k := 0; k < 2; k++ {
				cell := p*2 + k
				r := cell / yCols
				c := cell % yCols
				py := topY + r*bs + pad
				px := mx + c*bs + pad
				sum := 0
				for dy := 0; dy < core; dy++ {
					off := (py+dy)*stride + px
					for dx := 0; dx < core; dx++ {
						sum += int(Y[off+dx])
					}
				}
				avg := float64(sum) / coreArea
				lvl := 0
				for lvl < 5 && avg >= cal.yThresh6[lvl] {
					lvl++
				}
				lvls[k] = lvl
			}
			v := lvls[0]*6 + lvls[1]
			if v > 31 {
				v = 31
			}
			for b := 4; b >= 0; b-- {
				putBit(byte((v >> uint(b)) & 1))
			}
		}
	} else {
		for r := 0; r < yRows; r++ {
			py := topY + r*bs + pad
			for c := 0; c < yCols; c++ {
				px := mx + c*bs + pad
				sum := 0
				for dy := 0; dy < core; dy++ {
					off := (py+dy)*stride + px
					for dx := 0; dx < core; dx++ {
						sum += int(Y[off+dx])
					}
				}
				avg := float64(sum) / coreArea
				switch prof.YLevels {
				case 8:
					lvl := 0
					for lvl < 7 && avg >= cal.yThresh8[lvl] {
						lvl++
					}
					g := byte(lvl ^ (lvl >> 1))
					putBit((g >> 2) & 1)
					putBit((g >> 1) & 1)
					putBit(g & 1)
				case 4:
					lvl := 0
					if avg >= cal.yThresh4[2] {
						lvl = 3
					} else if avg >= cal.yThresh4[1] {
						lvl = 2
					} else if avg >= cal.yThresh4[0] {
						lvl = 1
					}
					g := levelToGray2[lvl]
					putBit((g >> 1) & 1)
					putBit(g & 1)
				default:
					if avg >= cal.yThresh2 {
						putBit(1)
					} else {
						putBit(0)
					}
				}
			}
		}
	}

	if prof.UseChroma {
		cCols := dw / prof.ChromaBlockY
		cRows := dh / prof.ChromaBlockY
		cbs := prof.ChromaBlockY / 2
		cPad := 1
		if cbs >= 8 {
			cPad = 2
		}
		cCore := cbs - 2*cPad
		if cCore < 1 {
			cPad = 0
			cCore = cbs
		}
		cArea := float64(cCore * cCore)
		U := src.UPlane()
		V := src.VPlane()
		uvStride := src.Width / 2
		cMarginX := mx / 2
		cTopY := topY / 2

		if prof.ArtMode == "video-fractal-chroma" {
			// Blind decode: regenerate ONLY the deterministic theme chroma field (host video NOT needed!)
			cLo, cHi := 48, 208
			if prof.ChromaLevels == 4 {
				cLo, cHi = 56, 200
			}
			baseCb, baseCr := RenderThemeChromaFields(uvStride, src.Height/2, int(hdr.FrameIndex), hdr.ContainerID, hdr.ArtTheme, cLo, cHi)
			if prof.ChromaLevels == 4 {
				for r := 0; r < cRows; r++ {
					cy := cTopY + r*cbs + cPad
					for c := 0; c < cCols; c++ {
						cx := cMarginX + c*cbs + cPad
						diffU, diffV := 0, 0
						for dy := 0; dy < cCore; dy++ {
							off := (cy+dy)*uvStride + cx
							for dx := 0; dx < cCore; dx++ {
								diffU += int(U[off+dx]) - int(baseCb[off+dx])
								diffV += int(V[off+dx]) - int(baseCr[off+dx])
							}
						}
						avgU := float64(diffU) / cArea
						uLvl := 0
						if avgU >= 22.0 {
							uLvl = 3
						} else if avgU >= 0.0 {
							uLvl = 2
						} else if avgU >= -22.0 {
							uLvl = 1
						}
						gu := levelToGray2[uLvl]
						putBit((gu >> 1) & 1)
						putBit(gu & 1)

						avgV := float64(diffV) / cArea
						vLvl := 0
						if avgV >= 22.0 {
							vLvl = 3
						} else if avgV >= 0.0 {
							vLvl = 2
						} else if avgV >= -22.0 {
							vLvl = 1
						}
						gv := levelToGray2[vLvl]
						putBit((gv >> 1) & 1)
						putBit(gv & 1)
					}
				}
			} else {
				for r := 0; r < cRows; r++ {
					cy := cTopY + r*cbs + cPad
					for c := 0; c < cCols; c++ {
						cx := cMarginX + c*cbs + cPad
						diffU, diffV := 0, 0
						for dy := 0; dy < cCore; dy++ {
							off := (cy+dy)*uvStride + cx
							for dx := 0; dx < cCore; dx++ {
								diffU += int(U[off+dx]) - int(baseCb[off+dx])
								diffV += int(V[off+dx]) - int(baseCr[off+dx])
							}
						}
						if diffU >= 0 {
							putBit(1)
						} else {
							putBit(0)
						}
						if diffV >= 0 {
							putBit(1)
						} else {
							putBit(0)
						}
					}
				}
			}
		} else if prof.ChromaLevels == 4 {
			for r := 0; r < cRows; r++ {
				cy := cTopY + r*cbs + cPad
				for c := 0; c < cCols; c++ {
					cx := cMarginX + c*cbs + cPad
					sumU, sumV := 0, 0
					for dy := 0; dy < cCore; dy++ {
						off := (cy+dy)*uvStride + cx
						for dx := 0; dx < cCore; dx++ {
							sumU += int(U[off+dx])
							sumV += int(V[off+dx])
						}
					}
					avgU := float64(sumU) / cArea
					uLvl := 0
					if avgU >= 184.0 {
						uLvl = 3
					} else if avgU >= 128.0 {
						uLvl = 2
					} else if avgU >= 72.0 {
						uLvl = 1
					}
					gu := levelToGray2[uLvl]
					putBit((gu >> 1) & 1)
					putBit(gu & 1)

					avgV := float64(sumV) / cArea
					vLvl := 0
					if avgV >= 184.0 {
						vLvl = 3
					} else if avgV >= 128.0 {
						vLvl = 2
					} else if avgV >= 72.0 {
						vLvl = 1
					}
					gv := levelToGray2[vLvl]
					putBit((gv >> 1) & 1)
					putBit(gv & 1)
				}
			}
		} else {
			for r := 0; r < cRows; r++ {
				cy := cTopY + r*cbs + cPad
				for c := 0; c < cCols; c++ {
					cx := cMarginX + c*cbs + cPad
					sumU, sumV := 0, 0
					for dy := 0; dy < cCore; dy++ {
						off := (cy+dy)*uvStride + cx
						for dx := 0; dx < cCore; dx++ {
							sumU += int(U[off+dx])
							sumV += int(V[off+dx])
						}
					}
					if float64(sumU)/cArea >= cal.cbThresh {
						putBit(1)
					} else {
						putBit(0)
					}
					if float64(sumV)/cArea >= cal.crThresh {
						putBit(1)
					} else {
						putBit(0)
					}
				}
			}
		}
	}

	rs := GetRSCodec(prof.RSDataBytes)
	recovered, corrected, err := rs.DecodeInterleaved(raw)
	if err != nil {
		return nil, fmt.Errorf("frame %d falhou na correção FEC: %w", hdr.FrameIndex, err)
	}
	if int(hdr.PayloadLen) > len(recovered) {
		return nil, fmt.Errorf("frame %d payload_len inválido (%d > %d)", hdr.FrameIndex, hdr.PayloadLen, len(recovered))
	}

	return &DecodeFrameResult{
		Header:         hdr,
		HeaderKind:     hdrKind,
		Payload:        recovered[:hdr.PayloadLen],
		CorrectedBytes: corrected,
		RawByteCount:   rawLen,
	}, nil
}

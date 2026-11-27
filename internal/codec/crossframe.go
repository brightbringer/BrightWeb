package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
)

// ============================================================================
// CROSS-FRAME REED-SOLOMON ERASURE CODING OVER GF(2^8) (Cauchy Matrix)
//
// Protects entire optical videos against catastrophic multi-second YouTube VBR
// bitrate blackouts (like a 20-25% drop at the end or middle of a video).
//
// Self-describing trailer ("YTCF", 32 bytes with CRC32) is appended right after
// [4B mLen][encryptedManifest] inside the manifest frames -> 100% backwards-
// compatible with all legacy videos and Manifest-First peek!
//
// Layout for K data partitions + M parity partitions (stripes = ceil(dataFrames/K)):
//   - Real data frames 0 .. dataFrames-1 live at relative index i = p*stripes + s
//     (virtual zero slots dataFrames .. K*stripes-1 are NOT written to the video!).
//   - Parity frames live at relative index dataFrames + q*stripes + s (q=0..M-1).
//   - Members of every stripe s are spaced ~stripes frames apart (~1/(K+M) of video),
//     so ANY contiguous blackout up to M/(K+M) of the video hits at most M members
//     per stripe -> 100% bit-exact recovery!
// ============================================================================

const (
	CrossFrameMagic       = "YTCF"
	CrossFrameVersion     = 1
	CrossFrameTrailerSize = 32
)

// CrossFrameSpec describes the exact cross-frame erasure coding geometry embedded in a video.
type CrossFrameSpec struct {
	K              int
	M              int
	Stripes        int
	DataFrames     int
	ChunkSize      int
	OrigPayloadLen uint64
}

func (s CrossFrameSpec) Enabled() bool {
	return s.K > 0 && s.M > 0 && s.Stripes > 0 && s.DataFrames > 0 && s.ChunkSize > 0
}

func (s CrossFrameSpec) ParityFrames() int {
	if !s.Enabled() {
		return 0
	}
	return s.M * s.Stripes
}

// MarshalTrailer serializes CrossFrameSpec into a 32-byte CRC32-protected YTCF block.
func (s CrossFrameSpec) MarshalTrailer() [CrossFrameTrailerSize]byte {
	var buf [CrossFrameTrailerSize]byte
	copy(buf[0:4], CrossFrameMagic)
	buf[4] = CrossFrameVersion
	buf[5] = byte(s.K)
	buf[6] = byte(s.M)
	buf[7] = 0
	binary.LittleEndian.PutUint32(buf[8:12], uint32(s.Stripes))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(s.DataFrames))
	binary.LittleEndian.PutUint32(buf[16:20], uint32(s.ChunkSize))
	binary.LittleEndian.PutUint64(buf[20:28], s.OrigPayloadLen)
	crc := crc32.ChecksumIEEE(buf[0:28])
	binary.LittleEndian.PutUint32(buf[28:32], crc)
	return buf
}

// ParseCrossFrameTrailer checks if b starts with a valid CRC32-verified YTCF block.
func ParseCrossFrameTrailer(b []byte) (CrossFrameSpec, bool) {
	if len(b) < CrossFrameTrailerSize || string(b[0:4]) != CrossFrameMagic {
		return CrossFrameSpec{}, false
	}
	if b[4] != CrossFrameVersion {
		return CrossFrameSpec{}, false
	}
	wantCRC := binary.LittleEndian.Uint32(b[28:32])
	if crc32.ChecksumIEEE(b[0:28]) != wantCRC {
		return CrossFrameSpec{}, false
	}
	spec := CrossFrameSpec{
		K:              int(b[5]),
		M:              int(b[6]),
		Stripes:        int(binary.LittleEndian.Uint32(b[8:12])),
		DataFrames:     int(binary.LittleEndian.Uint32(b[12:16])),
		ChunkSize:      int(binary.LittleEndian.Uint32(b[16:20])),
		OrigPayloadLen: binary.LittleEndian.Uint64(b[20:28]),
	}
	if !spec.Enabled() || spec.K+spec.M > 64 {
		return CrossFrameSpec{}, false
	}
	return spec, true
}

// ResolveCrossFrameParams chooses (K, M) automatically or according to user override:
//   - "off"/"none": disabled
//   - "high"/"k8m2": K=8, M=2 (+25% duration, survives 20% blackout or 2x10% blackouts)
//   - "max"/"k6m2"/"on": K=6, M=2 (+33% duration, survives 25% blackout or 2x12.5% blackouts)
//   - ""/"auto": activates K=6, M=2 on Stealth/Art modes with >= 24 data frames (immune to YouTube VBR drops!),
//     disabled on short or pure-data videos to keep raw backups maximally compact.
func ResolveCrossFrameParams(mode string, prof Profile, dataFrames, spreadStride int) (int, int) {
	if dataFrames < 6 || spreadStride > 1 {
		return 0, 0
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off", "none", "0", "false":
		return 0, 0
	case "high", "k8m2", "25":
		if dataFrames < 8 {
			return dataFrames, 2
		}
		return 8, 2
	case "max", "k6m2", "on", "true", "33":
		return 6, 2
	default: // "" or "auto"
		if prof.ArtMode != "" && dataFrames >= 24 {
			return 6, 2
		}
		return 0, 0
	}
}

// gfCauchyMatrix builds an M x K Cauchy matrix over GF(2^8) (every K x K submatrix is invertible).
func gfCauchyMatrix(k, m int) [][]byte {
	mat := make([][]byte, m)
	for i := 0; i < m; i++ {
		mat[i] = make([]byte, k)
		xi := byte(i)
		for j := 0; j < k; j++ {
			yj := byte(m + j)
			mat[i][j] = gfInv(xi ^ yj)
		}
	}
	return mat
}

// gfInvertMatrix inverts a square n x n matrix over GF(2^8) via Gauss-Jordan elimination.
func gfInvertMatrix(a [][]byte) ([][]byte, error) {
	n := len(a)
	aug := make([][]byte, n)
	for i := 0; i < n; i++ {
		aug[i] = make([]byte, 2*n)
		copy(aug[i][:n], a[i])
		aug[i][n+i] = 1
	}
	for col := 0; col < n; col++ {
		piv := -1
		for r := col; r < n; r++ {
			if aug[r][col] != 0 {
				piv = r
				break
			}
		}
		if piv < 0 {
			return nil, fmt.Errorf("matriz GF(2^8) singular na coluna %d", col)
		}
		if piv != col {
			aug[col], aug[piv] = aug[piv], aug[col]
		}
		invP := gfInv(aug[col][col])
		for c := 0; c < 2*n; c++ {
			aug[col][c] = gfMul(aug[col][c], invP)
		}
		for r := 0; r < n; r++ {
			if r == col || aug[r][col] == 0 {
				continue
			}
			f := aug[r][col]
			for c := 0; c < 2*n; c++ {
				aug[r][c] ^= gfMul(f, aug[col][c])
			}
		}
	}
	inv := make([][]byte, n)
	for i := 0; i < n; i++ {
		inv[i] = make([]byte, n)
		copy(inv[i], aug[i][n:])
	}
	return inv, nil
}

// BuildCrossFrameParityChunks computes M*stripes parity frames of length chunkSize for encryptedPayload,
// using virtual zero padding when dataFrames is not a multiple of K (zero wasted empty frames!).
func BuildCrossFrameParityChunks(encryptedPayload []byte, chunkSize, k, m int) ([][]byte, CrossFrameSpec) {
	dataFrames := (len(encryptedPayload) + chunkSize - 1) / chunkSize
	if dataFrames <= 0 || k <= 0 || m <= 0 || chunkSize <= 0 {
		return nil, CrossFrameSpec{}
	}
	stripes := (dataFrames + k - 1) / k
	spec := CrossFrameSpec{
		K:              k,
		M:              m,
		Stripes:        stripes,
		DataFrames:     dataFrames,
		ChunkSize:      chunkSize,
		OrigPayloadLen: uint64(len(encryptedPayload)),
	}
	mat := gfCauchyMatrix(k, m)
	parity := make([][]byte, m*stripes)

	for s := 0; s < stripes; s++ {
		for q := 0; q < m; q++ {
			out := make([]byte, chunkSize)
			for p := 0; p < k; p++ {
				dataIdx := p*stripes + s
				if dataIdx >= dataFrames {
					continue // virtual zero slot: XOR with 0 is a no-op!
				}
				coef := mat[q][p]
				if coef == 0 {
					continue
				}
				off := dataIdx * chunkSize
				end := off + chunkSize
				if end > len(encryptedPayload) {
					end = len(encryptedPayload)
				}
				src := encryptedPayload[off:end]
				for idx := 0; idx < len(src); idx++ {
					if src[idx] != 0 {
						out[idx] ^= gfMul(coef, src[idx])
					}
				}
			}
			parity[q*stripes+s] = out
		}
	}
	return parity, spec
}

// CanReconstructCrossFrame returns true as soon as EVERY stripe has >= K available chunks
// (counting virtual zero padding slots as always present). Allows early termination of FFmpeg!
func CanReconstructCrossFrame(spec CrossFrameSpec, seen map[uint32][]byte, startIdx uint32) bool {
	if !spec.Enabled() {
		return false
	}
	for s := 0; s < spec.Stripes; s++ {
		have := 0
		for p := 0; p < spec.K; p++ {
			dataIdx := p*spec.Stripes + s
			if dataIdx >= spec.DataFrames {
				have++ // virtual zero slot is always known!
				continue
			}
			if _, ok := seen[startIdx+uint32(dataIdx)]; ok {
				have++
			}
		}
		if have >= spec.K {
			continue
		}
		for q := 0; q < spec.M && have < spec.K; q++ {
			parIdx := spec.DataFrames + q*spec.Stripes + s
			if _, ok := seen[startIdx+uint32(parIdx)]; ok {
				have++
			}
		}
		if have < spec.K {
			return false
		}
	}
	return true
}

// ReconstructCrossFramePayload reassembles the exact OrigPayloadLen bytes from seen frames,
// mathematically reconstructing any missing/corrupted data frames via GF(2^8) Cauchy inversion!
func ReconstructCrossFramePayload(spec CrossFrameSpec, seen map[uint32][]byte, startIdx uint32) ([]byte, int, error) {
	if !spec.Enabled() {
		return nil, 0, errors.New("especificação Cross-Frame RS inválida")
	}
	k, m, stripes, chunkSize := spec.K, spec.M, spec.Stripes, spec.ChunkSize
	cMat := gfCauchyMatrix(k, m)
	zeroChunk := make([]byte, chunkSize)

	recoveredChunks := make([][]byte, spec.DataFrames)
	totalRecoveredFrames := 0

	getPadded := func(raw []byte) []byte {
		if len(raw) == chunkSize {
			return raw
		}
		buf := make([]byte, chunkSize)
		copy(buf, raw)
		return buf
	}

	for s := 0; s < stripes; s++ {
		present := make(map[int][]byte, k+m)
		missingData := 0
		for p := 0; p < k; p++ {
			dataIdx := p*stripes + s
			if dataIdx >= spec.DataFrames {
				present[p] = zeroChunk // virtual zero slot
				continue
			}
			if chunk, ok := seen[startIdx+uint32(dataIdx)]; ok {
				present[p] = getPadded(chunk)
			} else {
				missingData++
			}
		}
		if missingData == 0 {
			for p := 0; p < k; p++ {
				dataIdx := p*stripes + s
				if dataIdx < spec.DataFrames {
					recoveredChunks[dataIdx] = present[p]
				}
			}
			continue
		}

		for q := 0; q < m; q++ {
			parIdx := spec.DataFrames + q*stripes + s
			if chunk, ok := seen[startIdx+uint32(parIdx)]; ok {
				present[k+q] = getPadded(chunk)
			}
		}
		if len(present) < k {
			return nil, totalRecoveredFrames, fmt.Errorf("stripe %d/%d irrecuperável (%d/%d frames disponíveis — apagão excedeu tolerância %d/%d)",
				s, stripes, len(present), k, m, k+m)
		}

		// Choose K surviving rows (all present data first, then parity)
		chosen := make([]int, 0, k)
		for p := 0; p < k; p++ {
			if _, ok := present[p]; ok {
				chosen = append(chosen, p)
			}
		}
		for q := 0; q < m && len(chosen) < k; q++ {
			if _, ok := present[k+q]; ok {
				chosen = append(chosen, k+q)
			}
		}

		sub := make([][]byte, k)
		for row, idx := range chosen {
			sub[row] = make([]byte, k)
			if idx < k {
				sub[row][idx] = 1
			} else {
				copy(sub[row], cMat[idx-k])
			}
		}
		inv, err := gfInvertMatrix(sub)
		if err != nil {
			return nil, totalRecoveredFrames, err
		}

		for p := 0; p < k; p++ {
			dataIdx := p*stripes + s
			if dataIdx >= spec.DataFrames {
				continue
			}
			if existing, ok := present[p]; ok {
				recoveredChunks[dataIdx] = existing
				continue
			}
			rec := make([]byte, chunkSize)
			for r := 0; r < k; r++ {
				coef := inv[p][r]
				if coef == 0 {
					continue
				}
				src := present[chosen[r]]
				for b := 0; b < chunkSize; b++ {
					if src[b] != 0 {
						rec[b] ^= gfMul(coef, src[b])
					}
				}
			}
			recoveredChunks[dataIdx] = rec
			totalRecoveredFrames++
		}
	}

	out := make([]byte, 0, spec.OrigPayloadLen)
	rem := int64(spec.OrigPayloadLen)
	for i := 0; i < spec.DataFrames && rem > 0; i++ {
		c := recoveredChunks[i]
		take := int64(len(c))
		if take > rem {
			take = rem
		}
		out = append(out, c[:take]...)
		rem -= take
	}
	return out, totalRecoveredFrames, nil
}

// frameRange represents a half-open [start, end) frame partition for parallel encode/decode.
type frameRange struct {
	start int
	end   int
}

func splitUniformRanges(total, parts int) []frameRange {
	if parts < 1 {
		parts = 1
	}
	if parts > total {
		parts = total
	}
	out := make([]frameRange, 0, parts)
	for i := 0; i < parts; i++ {
		s := (i * total) / parts
		e := ((i + 1) * total) / parts
		if e > s {
			out = append(out, frameRange{start: s, end: e})
		}
	}
	return out
}

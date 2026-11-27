package codec

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// ============================================================================
// CROSS-FRAME REED-SOLOMON ERASURE CODING OVER GF(2^8) (Cauchy Matrix)
// Given K data frames of length L bytes each, generates M parity frames so ANY
// K surviving frames out of K+M (even after losing M contiguous partitions = 25%+
// of the entire video!) reconstruct 100% of all K original data frames bit-for-bit!
// ============================================================================

// cauchyMatrix builds an M x K Cauchy matrix over GF(2^8) where every square submatrix is invertible.
// Uses x_i = i (0..M-1), y_j = M + j (M..M+K-1), entry (i,j) = 1 / (x_i ^ y_j).
func cauchyMatrix(k, m int) [][]byte {
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

// encodeCrossFrameParity takes K equal-length data chunks and produces M parity chunks.
func encodeCrossFrameParity(data [][]byte, m int) [][]byte {
	k := len(data)
	if k == 0 || m <= 0 {
		return nil
	}
	chunkLen := len(data[0])
	mat := cauchyMatrix(k, m)
	parity := make([][]byte, m)
	for p := 0; p < m; p++ {
		out := make([]byte, chunkLen)
		for j := 0; j < k; j++ {
			coef := mat[p][j]
			if coef == 0 {
				continue
			}
			src := data[j]
			for idx := 0; idx < chunkLen; idx++ {
				if src[idx] != 0 {
					out[idx] ^= gfMul(coef, src[idx])
				}
			}
		}
		parity[p] = out
	}
	return parity
}

// invertGFMatrix inverts a square n x n matrix over GF(2^8) using Gauss-Jordan elimination.
func invertGFMatrix(a [][]byte) ([][]byte, error) {
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
			return nil, fmt.Errorf("matriz singular na coluna %d", col)
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

// reconstructCrossFrameData reconstructs all K original data chunks from ANY >= K present chunks
// out of total K+M (indices 0..K-1 = data, K..K+M-1 = parity).
func reconstructCrossFrameData(k, m int, present map[int][]byte) ([][]byte, int, error) {
	if len(present) < k {
		return nil, 0, fmt.Errorf("frames insuficientes no stripe (%d/%d disponíveis)", len(present), k)
	}
	// Fast path: all K data chunks already present!
	allData := true
	for i := 0; i < k; i++ {
		if _, ok := present[i]; !ok {
			allData = false
			break
		}
	}
	if allData {
		out := make([][]byte, k)
		for i := 0; i < k; i++ {
			out[i] = present[i]
		}
		return out, 0, nil
	}

	// Pick K surviving indices (prefer existing data chunks first, then parity chunks)
	chosen := make([]int, 0, k)
	missingData := 0
	for i := 0; i < k; i++ {
		if _, ok := present[i]; ok {
			chosen = append(chosen, i)
		} else {
			missingData++
		}
	}
	for p := 0; p < m && len(chosen) < k; p++ {
		if _, ok := present[k+p]; ok {
			chosen = append(chosen, k+p)
		}
	}
	if len(chosen) < k {
		return nil, 0, fmt.Errorf("stripe irrecuperável (%d/%d)", len(chosen), k)
	}

	// Build the K x K encoding submatrix for the K chosen rows
	cMat := cauchyMatrix(k, m)
	sub := make([][]byte, k)
	for row, idx := range chosen {
		sub[row] = make([]byte, k)
		if idx < k {
			sub[row][idx] = 1
		} else {
			copy(sub[row], cMat[idx-k])
		}
	}
	inv, err := invertGFMatrix(sub)
	if err != nil {
		return nil, 0, err
	}

	chunkLen := len(present[chosen[0]])
	out := make([][]byte, k)
	for i := 0; i < k; i++ {
		if existing, ok := present[i]; ok {
			out[i] = existing
			continue
		}
		rec := make([]byte, chunkLen)
		for r := 0; r < k; r++ {
			coef := inv[i][r]
			if coef == 0 {
				continue
			}
			src := present[chosen[r]]
			for b := 0; b < chunkLen; b++ {
				if src[b] != 0 {
					rec[b] ^= gfMul(coef, src[b])
				}
			}
		}
		out[i] = rec
	}
	return out, missingData, nil
}

// ============================================================================
// PARTITIONED PARALLEL ENCODE & PARALLEL DECODE BENCHMARK HARNESS
// ============================================================================

type partRange struct {
	start int // inclusive frame index
	end   int // exclusive frame index
}

func makeUniformPartitions(total, parts int) []partRange {
	if parts < 1 {
		parts = 1
	}
	if parts > total {
		parts = total
	}
	out := make([]partRange, 0, parts)
	for i := 0; i < parts; i++ {
		s := (i * total) / parts
		e := ((i + 1) * total) / parts
		if e > s {
			out = append(out, partRange{start: s, end: e})
		}
	}
	return out
}

// makeGeometricPartitions splits total frames by geometric ratios (e.g. [1, 2, 4, 8] -> 6.7%, 13.3%, 26.7%, 53.3%)
// or reverse geometric ([8, 4, 2, 1]).
func makeGeometricPartitions(total int, weights []int) []partRange {
	sumW := 0
	for _, w := range weights {
		sumW += w
	}
	out := make([]partRange, 0, len(weights))
	accW := 0
	prev := 0
	for i, w := range weights {
		accW += w
		end := (accW * total) / sumW
		if i == len(weights)-1 {
			end = total
		}
		if end > prev {
			out = append(out, partRange{start: prev, end: end})
			prev = end
		}
	}
	return out
}

func encodePartitionedMP4(ffmpegPath, outPath string, prof Profile, w, h, fps int, vCodec string, chunks [][]byte, manifestCount int, parts []partRange) (time.Duration, int64, error) {
	t0 := time.Now()
	totalFrames := len(chunks)
	tmpDir := filepath.Dir(outPath)
	segPaths := make([]string, len(parts))

	threadsPerSeg := runtime.GOMAXPROCS(0) / len(parts)
	if threadsPerSeg < 1 {
		threadsPerSeg = 1
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(parts))

	for pi, pr := range parts {
		segPaths[pi] = filepath.Join(tmpDir, fmt.Sprintf("seg_%d_%d.mp4", pi, time.Now().UnixNano()))
		wg.Add(1)
		go func(idx int, r partRange, segOut string) {
			defer wg.Done()
			args := []string{
				"-y", "-loglevel", "error",
				"-f", "rawvideo", "-pix_fmt", "yuv420p",
				"-s", fmt.Sprintf("%dx%d", w, h),
				"-r", fmt.Sprintf("%d", fps),
				"-i", "pipe:0",
			}
			if vCodec == "vp9" {
				args = append(args,
					"-c:v", "libvpx-vp9", "-crf", "34", "-b:v", "0",
					"-deadline", "realtime", "-cpu-used", "6",
					"-row-mt", "1", "-threads", fmt.Sprintf("%d", threadsPerSeg),
					"-g", fmt.Sprintf("%d", fps*2),
					"-pix_fmt", "yuv420p",
				)
			} else {
				args = append(args,
					"-c:v", "libx264", "-preset", "veryfast", "-crf", "26",
					"-threads", fmt.Sprintf("%d", threadsPerSeg),
					"-g", fmt.Sprintf("%d", fps*2),
					"-keyint_min", fmt.Sprintf("%d", fps),
					"-pix_fmt", "yuv420p",
				)
			}
			args = append(args, segOut)
			cmd := exec.Command(ffmpegPath, args...)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				errCh <- err
				return
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				errCh <- err
				return
			}
			fr := NewYUVFrame(w, h)
			for fi := r.start; fi < r.end; fi++ {
				flags := uint8(0)
				if fi < manifestCount {
					flags |= FlagManifest
				}
				if fi == totalFrames-1 {
					flags |= FlagLastFrame
				}
				hdr := FrameHeader{
					Version:     2,
					ProfileID:   prof.ID,
					Flags:       flags,
					ContainerID: 0xABCDEF123456,
					FrameIndex:  uint32(fi),
					TotalFrames: uint32(totalFrames),
				}
				if err := EncodeFrame(fr, prof, hdr, chunks[fi]); err != nil {
					_ = stdin.Close()
					_ = cmd.Wait()
					errCh <- err
					return
				}
				if err := writeChunked(stdin, fr.Buf); err != nil {
					_ = stdin.Close()
					_ = cmd.Wait()
					errCh <- err
					return
				}
			}
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				errCh <- fmt.Errorf("seg %d encode failed: %v (%s)", idx, err, stderr.String())
			}
		}(pi, pr, segPaths[pi])
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		for _, sp := range segPaths {
			_ = os.Remove(sp)
		}
		return 0, 0, e
	}

	if len(segPaths) == 1 {
		_ = os.Remove(outPath)
		if err := os.Rename(segPaths[0], outPath); err != nil {
			return 0, 0, err
		}
	} else {
		listPath := filepath.Join(tmpDir, fmt.Sprintf("concat_%d.txt", time.Now().UnixNano()))
		var lb bytes.Buffer
		for _, sp := range segPaths {
			fmt.Fprintf(&lb, "file '%s'\n", filepath.ToSlash(sp))
		}
		if err := os.WriteFile(listPath, lb.Bytes(), 0o644); err != nil {
			return 0, 0, err
		}
		concatCmd := exec.Command(ffmpegPath, "-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy", outPath)
		if out, err := concatCmd.CombinedOutput(); err != nil {
			return 0, 0, fmt.Errorf("concat failed: %v (%s)", err, string(out))
		}
		_ = os.Remove(listPath)
		for _, sp := range segPaths {
			_ = os.Remove(sp)
		}
	}
	dur := time.Since(t0)
	fi, _ := os.Stat(outPath)
	var sz int64
	if fi != nil {
		sz = fi.Size()
	}
	return dur, sz, nil
}

// decodePartitionedMP4 launches len(parts) parallel FFmpeg readers via -ss / -t (with small overlap
// so keyframe snapping after YouTube re-encoding never drops a frame) and deduplicates by FrameIndex!
func decodePartitionedMP4(ffmpegPath, videoPath string, w, h, fps, totalFrames int, parts []partRange) (map[uint32][]byte, time.Duration, time.Duration, error) {
	t0 := time.Now()
	var (
		mu             sync.Mutex
		seen           = make(map[uint32][]byte, totalFrames)
		firstPartReady time.Duration
		wg             sync.WaitGroup
		errCh          = make(chan error, len(parts))
	)
	threadsPerSeg := runtime.GOMAXPROCS(0) / len(parts)
	if threadsPerSeg < 1 {
		threadsPerSeg = 1
	}

	for pi, pr := range parts {
		wg.Add(1)
		go func(idx int, r partRange) {
			defer wg.Done()
			// Seek slightly early (-2 frames) and add +0.5s tail padding so keyframe rounding NEVER skips a boundary frame!
			startSec := float64(r.start-2) / float64(fps)
			if startSec < 0 {
				startSec = 0
			}
			durSec := float64(r.end-r.start+4)/float64(fps) + 0.50
			args := []string{"-loglevel", "error", "-threads", fmt.Sprintf("%d", threadsPerSeg)}
			if idx > 0 && startSec > 0 {
				args = append(args, "-ss", fmt.Sprintf("%.3f", startSec))
			}
			args = append(args, "-i", videoPath)
			if idx < len(parts)-1 {
				args = append(args, "-t", fmt.Sprintf("%.3f", durSec))
			}
			args = append(args, "-f", "rawvideo", "-pix_fmt", "yuv420p", "pipe:1")
			cmd := exec.Command(ffmpegPath, args...)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				errCh <- err
				return
			}
			if err := cmd.Start(); err != nil {
				errCh <- err
				return
			}
			fr := NewYUVFrame(w, h)
			var hint DecodeHint
			gotInRange := 0
			wantInRange := r.end - r.start
			for {
				if err := readFullChunked(stdout, fr.Buf); err != nil {
					break
				}
				res, err := DecodeFrameWithHint(fr, hint)
				if err != nil {
					continue
				}
				if hint.HeaderKind == HeaderKindUnknown {
					hint = DecodeHint{HeaderKind: res.HeaderKind, ProfileID: res.Header.ProfileID}
				}
				fIdx := res.Header.FrameIndex
				if int(fIdx) >= r.start && int(fIdx) < r.end {
					buf := make([]byte, len(res.Payload))
					copy(buf, res.Payload)
					mu.Lock()
					if _, exists := seen[fIdx]; !exists {
						seen[fIdx] = buf
						gotInRange++
					}
					mu.Unlock()
					if gotInRange >= wantInRange {
						_ = cmd.Process.Kill()
						break
					}
				}
			}
			_ = cmd.Wait()
			if idx == 0 {
				mu.Lock()
				firstPartReady = time.Since(t0)
				mu.Unlock()
			}
		}(pi, pr)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		return nil, 0, 0, e
	}
	return seen, time.Since(t0), firstPartReady, nil
}

func TestPartitionedVideoAndCrossFrameFECLab(t *testing.T) {
	ffmpegPath := filepath.Join(os.Getenv("LOCALAPPDATA"), "YTCloud", "bin", "ffmpeg.exe")
	if _, err := os.Stat(ffmpegPath); err != nil {
		ffmpegPath = "ffmpeg"
		if _, err := exec.LookPath(ffmpegPath); err != nil {
			t.Skip("ffmpeg not found")
		}
	}

	prof := Profiles[ProfileY8Gray]
	res := Resolutions["2k"]
	fps := 15
	capPerFrame := prof.PayloadCapacityLayout(res.Width, res.Height, true)

	// Generate 240 data frames (16s of 2K 2560x1440 y8-gray optical data -> real-world BrightVault/BrightWeb scale!)
	const dataFrames = 240
	const manifestFrames = 1
	payload := make([]byte, dataFrames*capPerFrame)
	_, _ = rand.Read(payload)
	origHash := sha256.Sum256(payload)

	manifestChunk := make([]byte, capPerFrame)
	binary.LittleEndian.PutUint32(manifestChunk[0:4], 64)
	copy(manifestChunk[4:68], []byte("PARTITION_LAB_MANIFEST_OK"))

	baseChunks := make([][]byte, manifestFrames+dataFrames)
	baseChunks[0] = manifestChunk
	for i := 0; i < dataFrames; i++ {
		baseChunks[manifestFrames+i] = payload[i*capPerFrame : (i+1)*capPerFrame]
	}

	tmpDir := t.TempDir()
	t.Logf("=== PARTITION LAB: 2K (2560x1440) y8-gray | %d frames | %.2f MB payload | %d CPU threads ===",
		len(baseChunks), float64(len(payload))/(1024*1024), runtime.GOMAXPROCS(0))

	// ------------------------------------------------------------------------
	// EXPERIMENT 1: ENCODE & DECODE SPEED — Sequential vs Uniform P=2,4,8 vs Geometric
	// ------------------------------------------------------------------------
	configs := []struct {
		name  string
		parts []partRange
	}{
		{"Sequencial (1x)", makeUniformPartitions(len(baseChunks), 1)},
		{"Uniforme 2x (0, 0.5)", makeUniformPartitions(len(baseChunks), 2)},
		{"Uniforme 4x (0, 0.25, 0.5, 0.75)", makeUniformPartitions(len(baseChunks), 4)},
		{"Uniforme 8x (0..0.875)", makeUniformPartitions(len(baseChunks), 8)},
		{"Geométrica Crescente [1,2,4,8]", makeGeometricPartitions(len(baseChunks), []int{1, 2, 4, 8})},
		{"Geométrica Decrescente [8,4,2,1]", makeGeometricPartitions(len(baseChunks), []int{8, 4, 2, 1})},
	}

	var baseEncDur, baseDecDur time.Duration
	for idx, cfg := range configs {
		outMP4 := filepath.Join(tmpDir, fmt.Sprintf("bench_%d.mp4", idx))
		encDur, sz, err := encodePartitionedMP4(ffmpegPath, outMP4, prof, res.Width, res.Height, fps, "h264", baseChunks, manifestFrames, cfg.parts)
		if err != nil {
			t.Fatalf("%s encode failed: %v", cfg.name, err)
		}
		seen, decDur, firstReady, err := decodePartitionedMP4(ffmpegPath, outMP4, res.Width, res.Height, fps, len(baseChunks), cfg.parts)
		if err != nil {
			t.Fatalf("%s decode failed: %v", cfg.name, err)
		}
		if len(seen) != len(baseChunks) {
			t.Fatalf("%s decoded %d/%d frames", cfg.name, len(seen), len(baseChunks))
		}
		// Verify SHA-256 bit-exact match
		var rebuf bytes.Buffer
		for i := manifestFrames; i < len(baseChunks); i++ {
			rebuf.Write(seen[uint32(i)])
		}
		if sha256.Sum256(rebuf.Bytes()) != origHash {
			t.Fatalf("%s SHA-256 mismatch!", cfg.name)
		}
		if idx == 0 {
			baseEncDur = encDur
			baseDecDur = decDur
		}
		t.Logf("H.264 %-34s | Enc: %5.2fs (%4.2fx) | Dec: %5.2fs (%4.2fx) | 1ª Parte: %4.2fs | Size: %5.2f MB",
			cfg.name,
			encDur.Seconds(), baseEncDur.Seconds()/encDur.Seconds(),
			decDur.Seconds(), baseDecDur.Seconds()/decDur.Seconds(),
			firstReady.Seconds(),
			float64(sz)/(1024*1024))
	}

	// Test Single-Partition Selective Extraction (extract ONLY [0.50..0.75] of Uniform 4x!)
	out4x := filepath.Join(tmpDir, "bench_2.mp4")
	parts4 := makeUniformPartitions(len(baseChunks), 4)
	tSel := time.Now()
	selSeen, _, _, err := decodePartitionedMP4(ffmpegPath, out4x, res.Width, res.Height, fps, len(baseChunks), []partRange{parts4[2]})
	if err != nil {
		t.Fatalf("selective extract failed: %v", err)
	}
	t.Logf("Acesso Seletivo [0.50..0.75] (só 1/4)  | Dec: %5.2fs (%4.2fx mais rápido que ler tudo) | Frames: %d",
		time.Since(tSel).Seconds(), baseDecDur.Seconds()/time.Since(tSel).Seconds(), len(selSeen))

	// Also compare VP9 Sequential 1x vs Uniform 4x & 8x (VP9 is where single-process FFmpeg bottlenecks hardest!)
	vp9SeqPath := filepath.Join(tmpDir, "vp9_1x.webm")
	vp9Enc1, vp9Sz1, err := encodePartitionedMP4(ffmpegPath, vp9SeqPath, prof, res.Width, res.Height, fps, "vp9", baseChunks, manifestFrames, makeUniformPartitions(len(baseChunks), 1))
	if err == nil {
		for _, p := range []int{2, 4, 8} {
			vp9ParPath := filepath.Join(tmpDir, fmt.Sprintf("vp9_%dx.webm", p))
			vp9EncP, vp9SzP, errP := encodePartitionedMP4(ffmpegPath, vp9ParPath, prof, res.Width, res.Height, fps, "vp9", baseChunks, manifestFrames, makeUniformPartitions(len(baseChunks), p))
			if errP == nil {
				if p == 2 {
					t.Logf("VP9   Sequencial (1x)                    | Enc: %5.2fs (1.00x) | Size: %5.2f MB", vp9Enc1.Seconds(), float64(vp9Sz1)/(1024*1024))
				}
				t.Logf("VP9   Uniforme %dx                        | Enc: %5.2fs (%4.2fx) | Size: %5.2f MB", p, vp9EncP.Seconds(), vp9Enc1.Seconds()/vp9EncP.Seconds(), float64(vp9SzP)/(1024*1024))
			}
		}
	}

	// ------------------------------------------------------------------------
	// EXPERIMENT 2: CROSS-FRAME FEC (PARIDADE ENTRE PARTIÇÕES) vs YOUTUBE BLACKOUTS!
	// ------------------------------------------------------------------------
	// Build Cross-Frame RS schemes:
	//   A) K=4, M=1 (+25% overhead): 4 data partitions (0, 0.20, 0.40, 0.60) + 1 parity partition (0.80) -> survives ANY 1 full partition (20%) wiped!
	//   B) K=3, M=1 (+33% overhead): 3 data partitions + 1 parity -> survives ANY 25% contiguous blackout (exact jEbDHI2H-24)!
	//   C) K=8, M=2 (+25% overhead): 8 data partitions + 2 parity partitions (interleaved!) -> survives ANY 2 separate blackouts totaling 20%!
	//   D) K=6, M=2 (+33% overhead): 6 data + 2 parity -> survives ANY 25% contiguous blackout (2 full partitions!) OR 2 separate 12.5% blackouts!
	schemes := []struct {
		name string
		k, m int
	}{
		{"Cross-Frame RS (K=4, M=1 · +25% duração)", 4, 1},
		{"Cross-Frame RS (K=3, M=1 · +33% duração)", 3, 1},
		{"Cross-Frame RS (K=8, M=2 · +25% duração)", 8, 2},
		{"Cross-Frame RS (K=6, M=2 · +33% duração)", 6, 2},
	}

	for _, sc := range schemes {
		// Pad dataFrames to multiple of K
		stripes := (dataFrames + sc.k - 1) / sc.k
		totalParts := sc.k + sc.m
		// Frame placement across time: partition p (0..K+M-1) occupies frames [p*stripes .. (p+1)*stripes - 1]
		// Every stripe s takes 1 frame from EACH partition (spaced evenly at 0, 1/(K+M), 2/(K+M), ... across the whole video!)
		tFEC := time.Now()
		allSlots := make([][]byte, totalParts*stripes)
		for s := 0; s < stripes; s++ {
			dSlice := make([][]byte, sc.k)
			for p := 0; p < sc.k; p++ {
				srcIdx := p*stripes + s
				if srcIdx < dataFrames {
					dSlice[p] = payload[srcIdx*capPerFrame : (srcIdx+1)*capPerFrame]
				} else {
					dSlice[p] = make([]byte, capPerFrame)
				}
				allSlots[p*stripes+s] = dSlice[p]
			}
			par := encodeCrossFrameParity(dSlice, sc.m)
			for q := 0; q < sc.m; q++ {
				allSlots[(sc.k+q)*stripes+s] = par[q]
			}
		}
		fecEncTime := time.Since(tFEC)

		// Simulate 3 brutal YouTube destruction attacks on the full timeline [0..totalSlots-1]:
		totalSlots := len(allSlots)
		attacks := []struct {
			label   string
			destroy func(slot int) bool
		}{
			{"Apagão final 20% (estilo jEbDHI2H-24)", func(slot int) bool { return slot >= (totalSlots*80)/100 }},
			{"Apagão final 25% (extremo)", func(slot int) bool { return slot >= (totalSlots*75)/100 }},
			{"Apagão no meio 20% [0.40..0.60]", func(slot int) bool { return slot >= (totalSlots*40)/100 && slot < (totalSlots*60)/100 }},
			{"2 apagões simultâneos (10%+10%)", func(slot int) bool {
				return (slot >= (totalSlots*20)/100 && slot < (totalSlots*30)/100) || (slot >= (totalSlots*70)/100 && slot < (totalSlots*80)/100)
			}},
		}

		results := ""
		for _, atk := range attacks {
			tRec := time.Now()
			ok := true
			rebuilt := make([]byte, 0, len(payload))
			recFrames := 0
			destroyedTotal := 0
			for slot := 0; slot < totalSlots; slot++ {
				if atk.destroy(slot) {
					destroyedTotal++
				}
			}
			// Reconstruct each stripe independently!
			recoveredData := make([][]byte, sc.k*stripes)
			for s := 0; s < stripes; s++ {
				present := make(map[int][]byte, totalParts)
				for p := 0; p < totalParts; p++ {
					slot := p*stripes + s
					if !atk.destroy(slot) {
						present[p] = allSlots[slot]
					}
				}
				rec, missingN, rErr := reconstructCrossFrameData(sc.k, sc.m, present)
				if rErr != nil {
					ok = false
					break
				}
				recFrames += missingN
				for p := 0; p < sc.k; p++ {
					recoveredData[p*stripes+s] = rec[p]
				}
			}
			if ok {
				for i := 0; i < dataFrames; i++ {
					rebuilt = append(rebuilt, recoveredData[i]...)
				}
				if sha256.Sum256(rebuilt) != origHash {
					ok = false
				}
			}
			status := "FALHOU"
			if ok {
				status = fmt.Sprintf("100%% OK (%dms, %d/%d frames perdidos reconstruídos)", time.Since(tRec).Milliseconds(), recFrames, destroyedTotal)
			}
			results += fmt.Sprintf("\n      • %-36s => %s", atk.label, status)
		}
		t.Logf("%s | FEC encode: %dms%s", sc.name, fecEncTime.Milliseconds(), results)
	}
}

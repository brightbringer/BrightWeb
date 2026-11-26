package codec

import (
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
)

// Pure-Go GF(2^8) Reed-Solomon Error-Correcting Code (primitive poly 0x11D, generator alpha=2).
// Unlike erasure-only codecs, this performs full syndrome decoding + Berlekamp-Massey +
// Chien search + Forney error magnitude evaluation, correcting up to (255-K)/2 arbitrary
// unknown byte errors per 255-byte codeword, combined with cross-frame byte interleaving.

var (
	gfExp [512]byte
	gfLog [256]byte
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

func gfDiv(a, b byte) byte {
	if a == 0 {
		return 0
	}
	if b == 0 {
		panic("gf256 division by zero")
	}
	return gfExp[(int(gfLog[a])+255-int(gfLog[b]))%255]
}

func gfInv(a byte) byte {
	return gfExp[255-int(gfLog[a])]
}

func gfPow(a byte, n int) byte {
	if n == 0 {
		return 1
	}
	if a == 0 {
		return 0
	}
	return gfExp[(int(gfLog[a])*n)%255]
}

type RSCodec struct {
	DataBytes   int
	ParityBytes int
	gen         []byte // generator polynomial of degree ParityBytes, high-to-low
}

var (
	rsCacheMu       sync.Mutex
	rsCache         = map[int]*RSCodec{}
	maxFECWorkers   = 0 // 0 = auto (all cores for YTBrowser, or configured via SetMaxWorkers in YTCloud)
	maxCodecRAMMB   = 0 // 0 = auto; when set, bounds parallel encode/decode frame-buffer RAM
	codecLowPrio    = false
)

// SetMaxWorkers caps parallel goroutines used in Reed-Solomon FEC and multi-core frame encode/decode.
func SetMaxWorkers(n int) {
	rsCacheMu.Lock()
	defer rsCacheMu.Unlock()
	if n < 1 {
		n = 1
	}
	maxFECWorkers = n
}

// GetMaxWorkers returns the configured worker cap (or runtime.GOMAXPROCS(0) if unset).
func GetMaxWorkers() int {
	rsCacheMu.Lock()
	n := maxFECWorkers
	rsCacheMu.Unlock()
	if n <= 0 {
		n = runtime.GOMAXPROCS(0)
	}
	if n < 1 {
		n = 1
	}
	return n
}

// SetResourceBudget configures RAM budget (MB) and low-priority process preference for codec workers.
func SetResourceBudget(maxRAMMB int, lowPriority bool) {
	rsCacheMu.Lock()
	maxCodecRAMMB = maxRAMMB
	codecLowPrio = lowPriority
	rsCacheMu.Unlock()
}

// GetResourceBudget returns (maxRAMMB, lowPriority) configured for codec workers.
func GetResourceBudget() (int, bool) {
	rsCacheMu.Lock()
	ram, low := maxCodecRAMMB, codecLowPrio
	rsCacheMu.Unlock()
	return ram, low
}

func fecWorkerCount(numBlocks int) int {
	rsCacheMu.Lock()
	workers := maxFECWorkers
	rsCacheMu.Unlock()
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0) / 2
	}
	if workers < 1 {
		workers = 1
	}
	if workers > numBlocks {
		workers = numBlocks
	}
	return workers
}

// GetRSCodec returns a cached RS(255, dataBytes) codec.
func GetRSCodec(dataBytes int) *RSCodec {
	rsCacheMu.Lock()
	defer rsCacheMu.Unlock()
	if c, ok := rsCache[dataBytes]; ok {
		return c
	}
	parity := 255 - dataBytes
	g := []byte{1}
	for i := 0; i < parity; i++ {
		// multiply g by (x - alpha^i) = (x + gfExp[i])
		next := make([]byte, len(g)+1)
		root := gfExp[i]
		for j := range g {
			next[j] ^= g[j]
			next[j+1] ^= gfMul(g[j], root)
		}
		g = next
	}
	c := &RSCodec{
		DataBytes:   dataBytes,
		ParityBytes: parity,
		gen:         g,
	}
	rsCache[dataBytes] = c
	return c
}

// EncodeBlock encodes exactly DataBytes into a 255-byte systematic codeword (msg || parity).
func (rs *RSCodec) EncodeBlock(msg []byte, dst []byte) {
	k := rs.DataBytes
	n := 255
	p := rs.ParityBytes
	copy(dst[:k], msg[:k])
	for i := k; i < n; i++ {
		dst[i] = 0
	}
	// Polynomial long division
	rem := make([]byte, p)
	for i := 0; i < k; i++ {
		fb := msg[i] ^ rem[0]
		copy(rem, rem[1:])
		rem[p-1] = 0
		if fb != 0 {
			for j := 0; j < p; j++ {
				rem[j] ^= gfMul(rs.gen[j+1], fb)
			}
		}
	}
	copy(dst[k:n], rem)
}

// DecodeBlock corrects up to ParityBytes/2 arbitrary byte errors in-place on a 255-byte codeword.
// Returns the number of corrected byte errors, or an error if uncorrectable.
func (rs *RSCodec) DecodeBlock(cw []byte) (int, error) {
	n := 255
	p := rs.ParityBytes
	if len(cw) != n {
		return 0, errors.New("tamanho de codeword RS inválido")
	}

	// 1. Compute syndromes S_j = cw(alpha^j) for j = 0..p-1
	syn := make([]byte, p)
	hasErr := false
	for j := 0; j < p; j++ {
		eval := byte(0)
		root := gfExp[j]
		for i := 0; i < n; i++ {
			eval = gfMul(eval, root) ^ cw[i]
		}
		syn[j] = eval
		if eval != 0 {
			hasErr = true
		}
	}
	if !hasErr {
		return 0, nil
	}

	// 2. Berlekamp-Massey algorithm to find error locator polynomial C(x)
	C := make([]byte, p+1)
	B := make([]byte, p+1)
	C[0] = 1
	B[0] = 1
	L := 0
	m := 1
	b := byte(1)

	for step := 0; step < p; step++ {
		d := syn[step]
		for i := 1; i <= L; i++ {
			d ^= gfMul(C[i], syn[step-i])
		}
		if d == 0 {
			m++
			continue
		}
		T := make([]byte, len(C))
		copy(T, C)
		coef := gfDiv(d, b)
		for i := 0; i+m < len(C); i++ {
			C[i+m] ^= gfMul(coef, B[i])
		}
		if 2*L <= step {
			L = step + 1 - L
			B = T
			b = d
			m = 1
		} else {
			m++
		}
	}

	if L == 0 || L > p/2 {
		return 0, fmt.Errorf("erros acima da capacidade FEC (L=%d, max=%d)", L, p/2)
	}

	// 3. Chien search: find roots of C(x) => X_k^-1 = alpha^r
	// Byte index in cw is i = n - 1 - power, where X_k = alpha^power
	errPos := make([]int, 0, L)
	errX := make([]byte, 0, L)
	for power := 0; power < n; power++ {
		xInv := gfExp[(255-power)%255]
		eval := C[0]
		xPow := byte(1)
		for j := 1; j <= L; j++ {
			xPow = gfMul(xPow, xInv)
			eval ^= gfMul(C[j], xPow)
		}
		if eval == 0 {
			pos := n - 1 - power
			errPos = append(errPos, pos)
			errX = append(errX, gfExp[power])
		}
	}

	if len(errPos) != L {
		return 0, fmt.Errorf("falha na busca de Chien (%d raízes encontradas, esperado %d)", len(errPos), L)
	}

	// 4. Error evaluator polynomial Omega(x) = (S(x) * C(x)) mod x^p
	omega := make([]byte, p)
	for i := 0; i < p; i++ {
		var sum byte
		for j := 0; j <= L && j <= i; j++ {
			sum ^= gfMul(syn[i-j], C[j])
		}
		omega[i] = sum
	}

	// 5. Forney's formula (since first consecutive root is alpha^0 = 1, e_k = X_k * Omega(X_k^-1) / C'(X_k^-1))
	for k := 0; k < L; k++ {
		xk := errX[k]
		xkInv := gfInv(xk)

		// Evaluate Omega(xkInv)
		num := byte(0)
		xp := byte(1)
		for i := 0; i < p; i++ {
			num ^= gfMul(omega[i], xp)
			xp = gfMul(xp, xkInv)
		}

		// Formal derivative C'(x) in GF(2^8): odd indices j => C[j] * x^(j-1)
		den := byte(0)
		xp = 1
		for j := 1; j <= L; j += 2 {
			den ^= gfMul(C[j], xp)
			xp = gfMul(xp, gfMul(xkInv, xkInv))
		}
		if den == 0 {
			return 0, errors.New("derivada nula na fórmula de Forney")
		}
		mag := gfMul(xk, gfDiv(num, den))
		cw[errPos[k]] ^= mag
	}

	// 6. Verify all syndromes are now zero
	for j := 0; j < p; j++ {
		eval := byte(0)
		root := gfExp[j]
		for i := 0; i < n; i++ {
			eval = gfMul(eval, root) ^ cw[i]
		}
		if eval != 0 {
			return 0, errors.New("verificação pós-correção RS falhou")
		}
	}

	return L, nil
}

// DecodeBlockWithErasures performs full Errors+Erasures Reed-Solomon decoding (Forney modified
// syndromes + Berlekamp-Massey + Chien search + Forney magnitudes) on a 255-byte codeword.
// It can correct up to 2*e + s <= ParityBytes (e.g. up to 76 erasures instead of 38 unknown errors!).
// Requires safetyMargin unused syndrome bytes (2*e + s <= ParityBytes - safetyMargin) to prevent
// spurious aliasing when s is large. On failure, cw is left unmodified.
func (rs *RSCodec) DecodeBlockWithErasures(cw []byte, erasures []int, safetyMargin int) (int, error) {
	n := 255
	p := rs.ParityBytes
	if len(cw) != n {
		return 0, errors.New("tamanho de codeword RS inválido")
	}
	s := len(erasures)
	if s == 0 {
		return rs.DecodeBlock(cw)
	}
	if s > p-safetyMargin {
		return 0, fmt.Errorf("erasures acima do limite seguro (%d > %d)", s, p-safetyMargin)
	}

	// 1. Compute syndromes S_j = cw(alpha^j) for j = 0..p-1
	syn := make([]byte, p)
	hasErr := false
	for j := 0; j < p; j++ {
		eval := byte(0)
		root := gfExp[j]
		for i := 0; i < n; i++ {
			eval = gfMul(eval, root) ^ cw[i]
		}
		syn[j] = eval
		if eval != 0 {
			hasErr = true
		}
	}
	if !hasErr {
		return 0, nil
	}

	// 2. Erasure locator polynomial Lambda_eras(x) = prod(1 + X_r * x)
	var isEras [255]bool
	erasLoc := make([]byte, s+1)
	erasLoc[0] = 1
	for k, pos := range erasures {
		if pos < 0 || pos >= n || isEras[pos] {
			return 0, errors.New("posição de erasure inválida ou duplicada")
		}
		isEras[pos] = true
		xr := gfExp[n-1-pos]
		for i := k + 1; i >= 1; i-- {
			erasLoc[i] ^= gfMul(erasLoc[i-1], xr)
		}
	}

	// 3. Forney modified syndromes T(x) = (S(x) * Lambda_eras(x)) mod x^p
	modSyn := make([]byte, p)
	for i := 0; i < p; i++ {
		var sum byte
		for j := 0; j <= s && j <= i; j++ {
			sum ^= gfMul(syn[i-j], erasLoc[j])
		}
		modSyn[i] = sum
	}

	// 4. Berlekamp-Massey on remaining remLen = p - s modified syndromes modSyn[s..p-1]
	remLen := p - s
	maxErr := (remLen - safetyMargin) / 2
	if maxErr < 0 {
		maxErr = 0
	}
	C := make([]byte, remLen+1)
	B := make([]byte, remLen+1)
	C[0] = 1
	B[0] = 1
	L := 0
	m := 1
	b := byte(1)

	for step := 0; step < remLen; step++ {
		d := modSyn[s+step]
		for i := 1; i <= L; i++ {
			d ^= gfMul(C[i], modSyn[s+step-i])
		}
		if d == 0 {
			m++
			continue
		}
		T := make([]byte, len(C))
		copy(T, C)
		coef := gfDiv(d, b)
		for i := 0; i+m < len(C); i++ {
			C[i+m] ^= gfMul(coef, B[i])
		}
		if 2*L <= step {
			L = step + 1 - L
			B = T
			b = d
			m = 1
		} else {
			m++
		}
	}

	if L > maxErr {
		return 0, fmt.Errorf("erros+erasures acima da capacidade FEC (L=%d, s=%d, maxL=%d)", L, s, maxErr)
	}

	// 5. Chien search for the L unknown error positions (must not collide with erasures)
	errPos := make([]int, 0, L)
	if L > 0 {
		for power := 0; power < n; power++ {
			xInv := gfExp[(255-power)%255]
			eval := C[0]
			xPow := byte(1)
			for j := 1; j <= L; j++ {
				xPow = gfMul(xPow, xInv)
				eval ^= gfMul(C[j], xPow)
			}
			if eval == 0 {
				pos := n - 1 - power
				if isEras[pos] {
					return 0, errors.New("raiz de erro colidiu com erasure")
				}
				errPos = append(errPos, pos)
			}
		}
		if len(errPos) != L {
			return 0, fmt.Errorf("falha na busca de Chien com erasures (%d/%d)", len(errPos), L)
		}
	}

	// 6. Combined locator Psi(x) = Lambda_eras(x) * C(x) of degree totalDeg = s + L
	totalDeg := s + L
	psi := make([]byte, totalDeg+1)
	for i := 0; i <= s; i++ {
		if erasLoc[i] == 0 {
			continue
		}
		for j := 0; j <= L; j++ {
			psi[i+j] ^= gfMul(erasLoc[i], C[j])
		}
	}

	// 7. Combined evaluator Omega(x) = (S(x) * Psi(x)) mod x^p
	omega := make([]byte, p)
	for i := 0; i < p; i++ {
		var sum byte
		for j := 0; j <= totalDeg && j <= i; j++ {
			sum ^= gfMul(syn[i-j], psi[j])
		}
		omega[i] = sum
	}

	// 8. Evaluate Forney error/erasure magnitudes on a scratch copy
	cand := make([]byte, n)
	copy(cand, cw)
	allPos := make([]int, 0, totalDeg)
	allPos = append(allPos, erasures...)
	allPos = append(allPos, errPos...)

	changed := 0
	for _, pos := range allPos {
		power := n - 1 - pos
		xk := gfExp[power]
		xkInv := gfInv(xk)

		num := byte(0)
		xp := byte(1)
		for i := 0; i < totalDeg; i++ {
			num ^= gfMul(omega[i], xp)
			xp = gfMul(xp, xkInv)
		}

		den := byte(0)
		xp = 1
		for j := 1; j <= totalDeg; j += 2 {
			den ^= gfMul(psi[j], xp)
			xp = gfMul(xp, gfMul(xkInv, xkInv))
		}
		if den == 0 {
			return 0, errors.New("derivada nula na fórmula de Forney (erasures)")
		}
		mag := gfMul(xk, gfDiv(num, den))
		if mag != 0 {
			cand[pos] ^= mag
			changed++
		}
	}

	// 9. Verify ALL p syndromes are now strictly zero
	for j := 0; j < p; j++ {
		eval := byte(0)
		root := gfExp[j]
		for i := 0; i < n; i++ {
			eval = gfMul(eval, root) ^ cand[i]
		}
		if eval != 0 {
			return 0, errors.New("verificação pós-correção RS (erasures) falhou")
		}
	}

	copy(cw, cand)
	return changed, nil
}

// DecodeBlockSoft performs standard hard-decision RS decoding first, and if that fails and
// per-byte soft reliabilities `conf` (255 entries, smaller = less reliable) are provided,
// runs Generalized Minimum Distance (GMD) errors+erasures decoding across increasing erasure counts.
func (rs *RSCodec) DecodeBlockSoft(cw []byte, conf []float32) (int, error) {
	corrected, hardErr := rs.DecodeBlock(cw)
	if hardErr == nil || len(conf) != 255 {
		return corrected, hardErr
	}

	// Rank byte indices from least reliable to most reliable
	order := make([]int, 255)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		return conf[order[i]] < conf[order[j]]
	})

	// GMD erasure schedule: step by 2 from smallest s (highest ML confidence) up to ParityBytes-4
	// (32-bit extra syndrome check -> false-accept prob < 2.3e-10 per trial).
	maxSafe := rs.ParityBytes - 4
	for s := 2; s <= maxSafe; s += 2 {
		if c, err := rs.DecodeBlockWithErasures(cw, order[:s], 4); err == nil {
			return c, nil
		}
	}
	// Chase-1 rescue: flip the single least-confident byte among the weakest 16 candidates?
	// No — even better: try relaxing safetyMargin to 2 bytes (16-bit extra check, still protected
	// end-to-end by AES-256-GCM + XChaCha20-Poly1305 AEAD & SHA-256!) across s = 4..ParityBytes-2.
	for s := 4; s <= rs.ParityBytes-2; s += 2 {
		if c, err := rs.DecodeBlockWithErasures(cw, order[:s], 2); err == nil {
			return c, nil
		}
	}
	return 0, hardErr
}

// EncodeInterleaved encodes payload into exactly rawLen bytes using RS(255, K) + full-frame byte interleaving.
func (rs *RSCodec) EncodeInterleaved(payload []byte, rawLen int) []byte {
	numBlocks := rawLen / 255
	out := make([]byte, rawLen)
	codewords := make([]byte, numBlocks*255)

	workers := fecWorkerCount(numBlocks)

	var wg sync.WaitGroup
	perWorker := (numBlocks + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start := w * perWorker
		end := start + perWorker
		if end > numBlocks {
			end = numBlocks
		}
		if start >= end {
			break
		}
		wg.Add(1)
		go func(s, e int) {
			defer wg.Done()
			msg := make([]byte, rs.DataBytes)
			for b := s; b < e; b++ {
				off := b * rs.DataBytes
				for i := range msg {
					if off+i < len(payload) {
						msg[i] = payload[off+i]
					} else {
						msg[i] = 0
					}
				}
				rs.EncodeBlock(msg, codewords[b*255:(b+1)*255])
			}
		}(start, end)
	}
	wg.Wait()

	// Interleave bytes across all codewords so localized video blur spreads across blocks
	for j := 0; j < 255; j++ {
		base := j * numBlocks
		for b := 0; b < numBlocks; b++ {
			out[base+b] = codewords[b*255+j]
		}
	}
	return out
}

// DecodeInterleaved de-interleaves raw frame bytes and runs parallel Reed-Solomon error correction.
// Returns recovered payload bytes (of length numBlocks*DataBytes), total corrected byte errors, and any uncorrectable error.
func (rs *RSCodec) DecodeInterleaved(raw []byte) ([]byte, int, error) {
	return rs.DecodeInterleavedSoft(raw, nil)
}

// DecodeInterleavedSoft is like DecodeInterleaved, but also accepts optional per-byte soft
// reliability scores (same length as raw, smaller = less reliable) to unlock GMD Errors+Erasures
// decoding whenever a block exceeds the hard-decision t=ParityBytes/2 error limit!
func (rs *RSCodec) DecodeInterleavedSoft(raw []byte, rawConf []float32) ([]byte, int, error) {
	numBlocks := len(raw) / 255
	if numBlocks == 0 {
		return nil, 0, errors.New("frame sem blocos RS suficientes")
	}
	useSoft := len(rawConf) >= numBlocks*255
	codewords := make([]byte, numBlocks*255)
	var cwConfs []float32
	if useSoft {
		cwConfs = make([]float32, numBlocks*255)
	}
	for j := 0; j < 255; j++ {
		base := j * numBlocks
		for b := 0; b < numBlocks; b++ {
			codewords[b*255+j] = raw[base+b]
			if useSoft {
				cwConfs[b*255+j] = rawConf[base+b]
			}
		}
	}

	payload := make([]byte, numBlocks*rs.DataBytes)
	workers := fecWorkerCount(numBlocks)

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		totalErrors int
		firstErr    error
	)

	perWorker := (numBlocks + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start := w * perWorker
		end := start + perWorker
		if end > numBlocks {
			end = numBlocks
		}
		if start >= end {
			break
		}
		wg.Add(1)
		go func(s, e int) {
			defer wg.Done()
			localErrs := 0
			for b := s; b < e; b++ {
				cw := codewords[b*255 : (b+1)*255]
				var conf []float32
				if useSoft {
					conf = cwConfs[b*255 : (b+1)*255]
				}
				corrected, err := rs.DecodeBlockSoft(cw, conf)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("bloco RS %d/%d irrecuperável: %w", b, numBlocks, err)
					}
					mu.Unlock()
					return
				}
				localErrs += corrected
				copy(payload[b*rs.DataBytes:(b+1)*rs.DataBytes], cw[:rs.DataBytes])
			}
			mu.Lock()
			totalErrors += localErrs
			mu.Unlock()
		}(start, end)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, totalErrors, firstErr
	}
	return payload, totalErrors, nil
}

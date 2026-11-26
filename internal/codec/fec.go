package codec

import (
	"errors"
	"fmt"
	"runtime"
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
	numBlocks := len(raw) / 255
	if numBlocks == 0 {
		return nil, 0, errors.New("frame sem blocos RS suficientes")
	}
	codewords := make([]byte, numBlocks*255)
	for j := 0; j < 255; j++ {
		base := j * numBlocks
		for b := 0; b < numBlocks; b++ {
			codewords[b*255+j] = raw[base+b]
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
				corrected, err := rs.DecodeBlock(cw)
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

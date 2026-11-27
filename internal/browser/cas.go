package browser

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ytcloud/internal/crypto"
)

// ContentCache implements a BrightNet/3 Content-Addressed Storage (CAS / ETag) cache indexed by SHA-256.
// Shared assets (CSS, JS, fonts, images, WASM) and revisited sites load in 0 ms directly from verified local cache!
type ContentCache struct {
	dir      string
	maxBytes int64
	mu       sync.Mutex
	putCount int
}

// NewContentCache creates a bounded SHA-256 content cache inside baseDir.
func NewContentCache(baseDir string, maxBytes int64) *ContentCache {
	dir := filepath.Join(baseDir, "cas")
	_ = os.MkdirAll(dir, 0o755)
	if maxBytes <= 0 {
		maxBytes = 512 * 1024 * 1024 // 512 MB cap
	}
	return &ContentCache{dir: dir, maxBytes: maxBytes}
}

func isValidSHA256Hex(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		c := h[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (c *ContentCache) itemPath(sha string) string {
	return filepath.Join(c.dir, sha[:2], sha)
}

// Get retrieves a cached file by SHA-256 and strictly re-verifies its hash before returning.
func (c *ContentCache) Get(sha256Hex string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	sha := strings.ToLower(strings.TrimSpace(sha256Hex))
	if !isValidSHA256Hex(sha) {
		return nil, false
	}
	p := c.itemPath(sha)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	if crypto.SHA256Hex(data) != sha {
		_ = os.Remove(p)
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return data, true
}

// Put stores a verified file in the CAS cache atomically (skipping empty or >32 MB blobs).
func (c *ContentCache) Put(sha256Hex string, data []byte) {
	if c == nil || len(data) == 0 || len(data) > 32*1024*1024 {
		return
	}
	sha := strings.ToLower(strings.TrimSpace(sha256Hex))
	if !isValidSHA256Hex(sha) {
		return
	}
	p := c.itemPath(sha)
	if _, err := os.Stat(p); err == nil {
		return
	}
	sub := filepath.Dir(p)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return
	}
	c.mu.Lock()
	c.putCount++
	shouldPrune := c.putCount%64 == 0
	c.mu.Unlock()
	if shouldPrune {
		go c.prune()
	}
}

func (c *ContentCache) prune() {
	c.mu.Lock()
	defer c.mu.Unlock()
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	var entries []entry
	var total int64
	_ = filepath.Walk(c.dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || strings.HasSuffix(p, ".tmp") {
			return nil
		}
		entries = append(entries, entry{path: p, size: fi.Size(), mod: fi.ModTime()})
		total += fi.Size()
		return nil
	})
	if total <= c.maxBytes {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.Before(entries[j].mod) })
	target := (c.maxBytes * 8) / 10
	for _, e := range entries {
		if total <= target {
			break
		}
		if os.Remove(e.path) == nil {
			total -= e.size
		}
	}
}

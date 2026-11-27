package packer

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"ytcloud/internal/crypto"
	"ytcloud/internal/sysutil"
)

// PackedFileEntry describes one file inside a YTCloud/BrightVault video container manifest.
type PackedFileEntry struct {
	RelPath       string `json:"rel_path"`
	Name          string `json:"name"`
	MimeType      string `json:"mime_type"`
	OriginalSize  int64  `json:"original_size"`
	StoredSize    int64  `json:"stored_size"`
	Compressed    bool   `json:"compressed"`
	InSolidBlock  bool   `json:"in_solid_block,omitempty"` // if true, Offset is inside the decompressed Solid block
	SolidBlockID  int    `json:"solid_block_id,omitempty"` // 1-based index into ContainerManifest.SolidBlocks (0 = legacy single solid block)
	LayerDepth    int    `json:"layer_depth,omitempty"`    // folder hierarchy depth (0 = root, 1 = subdir, 2 = nested...)
	Transform     string `json:"transform,omitempty"`      // "", "delta-pcm16", "webp-lossless"
	Offset        int64  `json:"offset"`                   // byte offset in container payload blob (or inside Solid block)
	SHA256        string `json:"sha256"`
	StoredSHA256  string `json:"stored_sha256,omitempty"` // integrity hash for pixel-exact transforms like webp-lossless
	ModTime       string `json:"mod_time"`
	PreviewKind   string `json:"preview_kind,omitempty"`   // "image", "text", "video", "none"
	PreviewBase64 string `json:"preview_base64,omitempty"` // compact JPEG thumbnail or text snippet
	LocalPath     string `json:"-"`                        // not serialized inside YouTube manifest
}

// LayerSolidBlock describes one independent per-layer Solid Zstd block inside the payload.
// Grouping compressible text/code per folder layer preserves ~100% Solid compression while
// unlocking progressive root-first loading and partial blackout isolation!
type LayerSolidBlock struct {
	ID         int   `json:"id"` // 1-based ID matching PackedFileEntry.SolidBlockID
	LayerDepth int   `json:"layer_depth"`
	Offset     int64 `json:"offset"`
	StoredSize int64 `json:"stored_size"`
}

// ContainerManifest is embedded in the Manifest-First frames (first 2-5s of every video).
type ContainerManifest struct {
	Version         int               `json:"version"`
	ContainerID     string            `json:"container_id"`
	CreatedAt       string            `json:"created_at"`
	ProfileCode     string            `json:"profile_code"`
	Resolution      string            `json:"resolution"`
	TotalSize       int64             `json:"total_size"`
	SolidCompressed bool              `json:"solid_compressed,omitempty"` // legacy single solid block
	SolidOffset     int64             `json:"solid_offset,omitempty"`
	SolidStoredSize int64             `json:"solid_stored_size,omitempty"`
	SolidBlocks     []LayerSolidBlock `json:"solid_blocks,omitempty"` // v3 hybrid per-layer solid blocks
	Files           []PackedFileEntry `json:"files"`
	IsCatalog       bool              `json:"is_catalog,omitempty"`
	CatalogLabel    string            `json:"catalog_label,omitempty"`
	// Incremental Sync / Delta Chain metadata (file-level self-contained deltas, zero cascade corruption!)
	SyncID          string   `json:"sync_id,omitempty"`
	SyncFolderName  string   `json:"sync_folder_name,omitempty"`
	DeltaIndex      int      `json:"delta_index,omitempty"`     // 0 = Base full video, 1..MaxDeltas = Update video
	MaxDeltas       int      `json:"max_deltas,omitempty"`      // default 10
	ParentVideoID   string   `json:"parent_video_id,omitempty"` // previous YouTube VideoID in chain
	ParentVideoURL  string   `json:"parent_video_url,omitempty"`
	DeletedRelPaths []string `json:"deleted_rel_paths,omitempty"`
}

// PackOptions configures smart pre-encryption compression.
type PackOptions struct {
	ContainerID          string
	ProfileCode          string
	ResolutionCode       string
	FFmpegPath           string
	IncludeMicroPreviews bool
	OptimizePNGLossless  bool // pixel-exact PNG -> WebP Lossless via FFmpeg (-38% to -97%)
	OnProgress           func(doneFiles, totalFiles int, doneBytes, totalBytes int64)
}

var incompressibleExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true, ".avif": true,
	".mp4": true, ".mkv": true, ".mov": true, ".webm": true, ".avi": true,
	".mp3": true, ".aac": true, ".ogg": true, ".flac": true, ".m4a": true,
	".zip": true, ".7z": true, ".rar": true, ".gz": true, ".zst": true, ".xz": true,
	".docx": true, ".xlsx": true, ".pptx": true,
}

// InputFile represents a file selected for packing (either from disk or uploaded via browser).
type InputFile struct {
	RelPath   string
	LocalPath string // optional if Data is provided
	Data      []byte // optional if LocalPath is provided
	ModTime   string
}

// CollectInputsFromPath walks a local file or directory and returns all InputFiles.
func CollectInputsFromPath(targetPath string) ([]InputFile, error) {
	info, err := os.Stat(targetPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []InputFile{{
			RelPath:   filepath.Base(targetPath),
			LocalPath: targetPath,
			ModTime:   info.ModTime().Format("2006-01-02 15:04:05"),
		}}, nil
	}
	baseName := filepath.Base(targetPath)
	var out []InputFile
	err = filepath.Walk(targetPath, func(p string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fi.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(targetPath, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(filepath.Join(baseName, rel))
		out = append(out, InputFile{
			RelPath:   rel,
			LocalPath: p,
			ModTime:   fi.ModTime().Format("2006-01-02 15:04:05"),
		})
		return nil
	})
	return out, err
}

// parseWAVPCM16 locates the PCM16 "data" chunk of a RIFF/WAVE file (1 or 2 channels).
func parseWAVPCM16(wav []byte) (channels, dataOff, dataLen int, ok bool) {
	if len(wav) < 44 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return 0, 0, 0, false
	}
	pos := 12
	var audioFmt, bitsPerSample uint16
	var ch uint16
	foundFmt := false
	for pos+8 <= len(wav) {
		id := string(wav[pos : pos+4])
		sz := int(binary.LittleEndian.Uint32(wav[pos+4 : pos+8]))
		body := pos + 8
		if body+sz > len(wav) {
			break
		}
		if id == "fmt " && sz >= 16 {
			audioFmt = binary.LittleEndian.Uint16(wav[body : body+2])
			ch = binary.LittleEndian.Uint16(wav[body+2 : body+4])
			bitsPerSample = binary.LittleEndian.Uint16(wav[body+14 : body+16])
			foundFmt = true
		} else if id == "data" && foundFmt {
			if audioFmt == 1 && bitsPerSample == 16 && (ch == 1 || ch == 2) && sz >= 64 && sz%2 == 0 {
				return int(ch), body, sz, true
			}
			return 0, 0, 0, false
		}
		pos = body + sz
		if sz%2 == 1 {
			pos++
		}
	}
	return 0, 0, 0, false
}

// deltaEncodePCM16 applies a 100% bit-exact reversible per-channel delta + byte-plane split on WAV PCM16.
func deltaEncodePCM16(wav []byte) ([]byte, bool) {
	ch, dataOff, dataLen, ok := parseWAVPCM16(wav)
	if !ok {
		return nil, false
	}
	out := make([]byte, len(wav))
	copy(out[:dataOff], wav[:dataOff])
	copy(out[dataOff+dataLen:], wav[dataOff+dataLen:])

	pcm := wav[dataOff : dataOff+dataLen]
	nSamples := dataLen / 2
	hi := out[dataOff : dataOff+nSamples]
	lo := out[dataOff+nSamples : dataOff+dataLen]
	var prev [2]int16
	for i := 0; i < nSamples; i++ {
		c := i % ch
		cur := int16(binary.LittleEndian.Uint16(pcm[i*2 : i*2+2]))
		diff := cur - prev[c]
		prev[c] = cur
		u := uint16(diff)
		hi[i] = byte(u >> 8)
		lo[i] = byte(u)
	}
	return out, true
}

// deltaDecodePCM16 reverses deltaEncodePCM16 to recover the exact original WAV bytes & SHA-256.
func deltaDecodePCM16(encoded []byte) ([]byte, error) {
	ch, dataOff, dataLen, ok := parseWAVPCM16(encoded)
	if !ok {
		return nil, fmt.Errorf("cabeçalho WAV inválido na reversão delta-pcm16")
	}
	out := make([]byte, len(encoded))
	copy(out[:dataOff], encoded[:dataOff])
	copy(out[dataOff+dataLen:], encoded[dataOff+dataLen:])

	nSamples := dataLen / 2
	hi := encoded[dataOff : dataOff+nSamples]
	lo := encoded[dataOff+nSamples : dataOff+dataLen]
	dst := out[dataOff : dataOff+dataLen]
	var prev [2]int16
	for i := 0; i < nSamples; i++ {
		c := i % ch
		diff := int16((uint16(hi[i]) << 8) | uint16(lo[i]))
		cur := prev[c] + diff
		prev[c] = cur
		binary.LittleEndian.PutUint16(dst[i*2:i*2+2], uint16(cur))
	}
	return out, nil
}

func convertPNGToWebPLossless(ffmpegPath string, rawPNG []byte) ([]byte, error) {
	if ffmpegPath == "" {
		return nil, fmt.Errorf("ffmpeg indisponível")
	}
	tmpDir, err := os.MkdirTemp("", "ytc-png-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	inPath := filepath.Join(tmpDir, "in.png")
	outPath := filepath.Join(tmpDir, "out.webp")
	if err := os.WriteFile(inPath, rawPNG, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.Command(ffmpegPath, "-y", "-loglevel", "error", "-i", inPath, "-c:v", "libwebp", "-lossless", "1", "-compression_level", "6", outPath)
	sysutil.ApplySubprocessAttrs(cmd, true)
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(outPath)
}

func convertWebPLosslessToPNG(ffmpegPath string, rawWebP []byte) ([]byte, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	tmpDir, err := os.MkdirTemp("", "ytc-webp-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	inPath := filepath.Join(tmpDir, "in.webp")
	outPath := filepath.Join(tmpDir, "out.png")
	if err := os.WriteFile(inPath, rawWebP, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.Command(ffmpegPath, "-y", "-loglevel", "error", "-i", inPath, "-c:v", "png", outPath)
	sysutil.ApplySubprocessAttrs(cmd, true)
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(outPath)
}

// PackFiles packs inputs into a single contiguous payload blob + rich ContainerManifest (with previews).
// Optional onProgress reports (doneFiles, totalFiles, doneBytes, totalBytes) for live ETA calculation.
func PackFiles(inputs []InputFile, containerID, profileCode, resCode, ffmpegPath string, includeMicroPreviews bool, onProgress ...func(doneFiles, totalFiles int, doneBytes, totalBytes int64)) (*ContainerManifest, []byte, error) {
	var progressFn func(doneFiles, totalFiles int, doneBytes, totalBytes int64)
	if len(onProgress) > 0 {
		progressFn = onProgress[0]
	}
	return PackFilesWithOptions(inputs, PackOptions{
		ContainerID:          containerID,
		ProfileCode:          profileCode,
		ResolutionCode:       resCode,
		FFmpegPath:           ffmpegPath,
		IncludeMicroPreviews: includeMicroPreviews,
		OptimizePNGLossless:  false,
		OnProgress:           progressFn,
	})
}

// PackFilesWithOptions packs inputs using Solid zstd Best compression (sorted by extension),
// reversible bit-exact Delta-PCM16 for WAVs, and optional pixel-exact WebP Lossless for PNGs.
func PackFilesWithOptions(inputs []InputFile, opts PackOptions) (*ContainerManifest, []byte, error) {
	var totalInputBytes int64
	for _, inp := range inputs {
		if inp.Data != nil {
			totalInputBytes += int64(len(inp.Data))
		} else if inp.LocalPath != "" {
			if fi, err := os.Stat(inp.LocalPath); err == nil {
				totalInputBytes += fi.Size()
			}
		}
	}

	type preparedItem struct {
		entry       PackedFileEntry
		data        []byte
		ext         string
		wantsSolid  bool
	}

	items := make([]preparedItem, 0, len(inputs))
	var doneBytes int64
	for idx, inp := range inputs {
		raw := inp.Data
		var err error
		if raw == nil && inp.LocalPath != "" {
			raw, err = os.ReadFile(inp.LocalPath)
			if err != nil {
				return nil, nil, fmt.Errorf("erro lendo %s: %w", inp.LocalPath, err)
			}
		}

		name := filepath.Base(inp.RelPath)
		ext := strings.ToLower(filepath.Ext(name))
		mt := mime.TypeByExtension(ext)
		if mt == "" {
			mt = "application/octet-stream"
		}

		entry := PackedFileEntry{
			RelPath:      filepath.ToSlash(inp.RelPath),
			Name:         name,
			MimeType:     mt,
			OriginalSize: int64(len(raw)),
			SHA256:       crypto.SHA256Hex(raw),
			ModTime:      inp.ModTime,
			LocalPath:    inp.LocalPath,
		}
		if opts.IncludeMicroPreviews {
			kind, b64 := GeneratePreview(raw, inp.LocalPath, ext, mt, opts.FFmpegPath)
			entry.PreviewKind = kind
			entry.PreviewBase64 = b64
		}

		stored := raw
		wantsSolid := false
		if ext == ".wav" {
			if deltaBytes, ok := deltaEncodePCM16(raw); ok {
				stored = deltaBytes
				entry.Transform = "delta-pcm16"
				wantsSolid = true
			} else if len(raw) > 256 {
				wantsSolid = true
			}
		} else if ext == ".png" && opts.OptimizePNGLossless && opts.FFmpegPath != "" && len(raw) > 1024 {
			if webpBytes, err := convertPNGToWebPLossless(opts.FFmpegPath, raw); err == nil && len(webpBytes) < (len(raw)*9)/10 {
				stored = webpBytes
				entry.Transform = "webp-lossless"
				entry.StoredSHA256 = crypto.SHA256Hex(webpBytes)
			}
		} else if !incompressibleExts[ext] && len(raw) > 64 {
			wantsSolid = true
		}

		entry.StoredSize = int64(len(stored))
		items = append(items, preparedItem{
			entry:      entry,
			data:       stored,
			ext:        ext,
			wantsSolid: wantsSolid,
		})

		doneBytes += int64(len(raw))
		if opts.OnProgress != nil {
			opts.OnProgress(idx+1, len(inputs), doneBytes, totalInputBytes)
		}
	}

	// Compute normalized folder layer depths (strip shared top-level folder prefix if all inputs share one,
	// e.g. "MySite/index.html" -> depth 0, "MySite/pages/about.html" -> depth 1, "MySite/assets/img/a.jpg" -> depth 2).
	sharedRoot := ""
	if len(items) > 1 {
		firstSlash := strings.Index(items[0].entry.RelPath, "/")
		if firstSlash > 0 {
			cand := items[0].entry.RelPath[:firstSlash+1]
			allShare := true
			for i := 1; i < len(items); i++ {
				if !strings.HasPrefix(items[i].entry.RelPath, cand) {
					allShare = false
					break
				}
			}
			if allShare {
				sharedRoot = cand
			}
		}
	}

	maxDepth := 0
	for i := range items {
		rel := strings.TrimPrefix(items[i].entry.RelPath, sharedRoot)
		rel = strings.TrimPrefix(rel, "/")
		d := strings.Count(rel, "/")
		items[i].entry.LayerDepth = d
		if d > maxDepth {
			maxDepth = d
		}
	}

	// BrightNet/3 Critical-Path HTML Promotion (HTTP/2 Priority / Preload Hints):
	// Parse root index.html for <link href>, <script src>, and <img src> references and promote
	// referenced small assets (<= 512 KB, even inside subdirs like assets/css/main.css!) into Camada 0!
	var indexBytes []byte
	for i := range items {
		if items[i].entry.LayerDepth == 0 {
			lowName := strings.ToLower(items[i].entry.Name)
			if lowName == "index.html" || lowName == "index.htm" {
				indexBytes = items[i].data
				break
			}
		}
	}
	if len(indexBytes) > 0 && len(indexBytes) <= 2*1024*1024 {
		critRefs := extractCriticalHTMLRefs(string(indexBytes))
		if len(critRefs) > 0 {
			for i := range items {
				if items[i].entry.LayerDepth == 0 || items[i].entry.OriginalSize > 512*1024 {
					continue
				}
				rel := strings.TrimPrefix(items[i].entry.RelPath, sharedRoot)
				rel = strings.ToLower(strings.TrimPrefix(rel, "/"))
				if critRefs[rel] {
					items[i].entry.LayerDepth = 0
				}
			}
		}
	}

	// Critical-path priority score within each folder layer: index.html -> .html -> .css/.js/.json -> docs -> other
	layerPriority := func(it preparedItem) int {
		base := strings.ToLower(it.entry.Name)
		if it.entry.LayerDepth == 0 && (base == "index.html" || base == "index.htm") {
			return 100
		}
		switch it.ext {
		case ".html", ".htm":
			return 80
		case ".css", ".js", ".mjs", ".json", ".svg", ".woff2":
			return 70
		case ".md", ".txt", ".xml", ".yaml", ".yml", ".csv":
			return 50
		default:
			return 20
		}
	}

	manifest := &ContainerManifest{
		Version:     3,
		ContainerID: opts.ContainerID,
		ProfileCode: opts.ProfileCode,
		Resolution:  opts.ResolutionCode,
		Files:       make([]PackedFileEntry, len(items)),
	}

	var payload bytes.Buffer
	const maxLayerSolidRawBytes = 32 * 1024 * 1024 // Cap per-layer solid block at 32 MB for low RAM & fast random access

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderConcurrency(2))
	if err != nil {
		return nil, nil, err
	}
	defer enc.Close()

	// BrightNet/3 Brotli-inspired Zstd Web Dictionary encoder (shrinks HTML/CSS/JS/SVG/JSON by 15%-45%!)
	encWebDict, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
		zstd.WithEncoderConcurrency(2),
		zstd.WithEncoderDictRaw(WebDictV1ID, WebDictV1),
	)
	if err != nil {
		return nil, nil, err
	}
	defer encWebDict.Close()

	encodeBestZstd := func(raw []byte, tryWebDict bool) []byte {
		plain := enc.EncodeAll(raw, nil)
		if tryWebDict && len(raw) <= 2*1024*1024 {
			withDict := encWebDict.EncodeAll(raw, nil)
			if len(withDict) < len(plain) {
				return withDict
			}
		}
		return plain
	}

	// Hybrid Layer-by-Layer Packing (depth 0 -> 1 -> 2 -> ...):
	//   1) Compressible text/code files at depth d -> 1 mini Solid Zstd block (0% compression loss + instant root!)
	//   2) Incompressible binary/media files at depth d -> individual self-contained entries sorted Shortest-Job-First!
	nextBlockID := 1
	for d := 0; d <= maxDepth; d++ {
		var solidIdxs []int
		var binIdxs []int
		for i := range items {
			if items[i].entry.LayerDepth != d {
				continue
			}
			if items[i].wantsSolid {
				solidIdxs = append(solidIdxs, i)
			} else {
				binIdxs = append(binIdxs, i)
			}
		}

		// 1. Layer d compressible files: sort by (ext, critical priority desc, relPath) for maximum Zstd dictionary hits
		if len(solidIdxs) > 0 {
			sort.SliceStable(solidIdxs, func(a, b int) bool {
				ia, ib := solidIdxs[a], solidIdxs[b]
				pa, pb := layerPriority(items[ia]), layerPriority(items[ib])
				if pa != pb {
					return pa > pb
				}
				if items[ia].ext != items[ib].ext {
					return items[ia].ext < items[ib].ext
				}
				return items[ia].entry.RelPath < items[ib].entry.RelPath
			})

			var layerTotal int64
			for _, idx := range solidIdxs {
				layerTotal += int64(len(items[idx].data))
			}

			if layerTotal <= maxLayerSolidRawBytes {
				var solidRaw bytes.Buffer
				solidRaw.Grow(int(layerTotal))
				solidOffsets := make([]int64, len(solidIdxs))
				hasWebText := false
				for k, idx := range solidIdxs {
					solidOffsets[k] = int64(solidRaw.Len())
					solidRaw.Write(items[idx].data)
					switch items[idx].ext {
					case ".html", ".htm", ".css", ".js", ".mjs", ".json", ".svg", ".xml":
						hasWebText = true
					}
				}
				solidComp := encodeBestZstd(solidRaw.Bytes(), hasWebText)
				if len(solidComp) < solidRaw.Len()-32 {
					bid := nextBlockID
					nextBlockID++
					blkOff := int64(payload.Len())
					payload.Write(solidComp)
					manifest.SolidBlocks = append(manifest.SolidBlocks, LayerSolidBlock{
						ID:         bid,
						LayerDepth: d,
						Offset:     blkOff,
						StoredSize: int64(len(solidComp)),
					})
					for k, idx := range solidIdxs {
						items[idx].entry.InSolidBlock = true
						items[idx].entry.SolidBlockID = bid
						items[idx].entry.Compressed = true
						items[idx].entry.Offset = solidOffsets[k]
						items[idx].data = nil
					}
				} else {
					// Solid didn't save space; write individually
					for _, idx := range solidIdxs {
						items[idx].wantsSolid = false
						if items[idx].entry.Transform == "delta-pcm16" {
							if orig, dErr := deltaDecodePCM16(items[idx].data); dErr == nil {
								items[idx].data = orig
								items[idx].entry.Transform = ""
								items[idx].entry.StoredSize = int64(len(orig))
							}
						}
						items[idx].entry.Offset = int64(payload.Len())
						payload.Write(items[idx].data)
						items[idx].data = nil
					}
				}
				solidRaw.Reset()
			} else {
				// Huge layer (>32 MB compressible): compress per-file within this layer to keep RAM bounded
				for _, idx := range solidIdxs {
					comp := enc.EncodeAll(items[idx].data, nil)
					if len(comp) < len(items[idx].data)-64 {
						items[idx].data = comp
						items[idx].entry.Compressed = true
						items[idx].entry.StoredSize = int64(len(comp))
					} else if items[idx].entry.Transform == "delta-pcm16" {
						if orig, dErr := deltaDecodePCM16(items[idx].data); dErr == nil {
							items[idx].data = orig
							items[idx].entry.Transform = ""
							items[idx].entry.StoredSize = int64(len(orig))
						}
					}
					items[idx].entry.Offset = int64(payload.Len())
					payload.Write(items[idx].data)
					items[idx].data = nil
				}
			}
		}

		// 2. Layer d binary/media files: Shortest-Job-First (smallest first!) written individually for instant random access
		if len(binIdxs) > 0 {
			sort.SliceStable(binIdxs, func(a, b int) bool {
				ia, ib := binIdxs[a], binIdxs[b]
				if items[ia].entry.StoredSize != items[ib].entry.StoredSize {
					return items[ia].entry.StoredSize < items[ib].entry.StoredSize
				}
				return items[ia].entry.RelPath < items[ib].entry.RelPath
			})
			for _, idx := range binIdxs {
				items[idx].entry.Offset = int64(payload.Len())
				payload.Write(items[idx].data)
				items[idx].data = nil
			}
		}
	}

	// Preserve manifest.Files in layer-first order (C0 -> C1 -> C2...) for intuitive progressive inspection
	fileOrder := make([]int, len(items))
	for i := range fileOrder {
		fileOrder[i] = i
	}
	sort.SliceStable(fileOrder, func(a, b int) bool {
		ia, ib := items[fileOrder[a]].entry, items[fileOrder[b]].entry
		if ia.LayerDepth != ib.LayerDepth {
			return ia.LayerDepth < ib.LayerDepth
		}
		if ia.InSolidBlock != ib.InSolidBlock {
			return ia.InSolidBlock
		}
		return ia.Offset < ib.Offset
	})
	for outIdx, srcIdx := range fileOrder {
		manifest.TotalSize += items[srcIdx].entry.OriginalSize
		manifest.Files[outIdx] = items[srcIdx].entry
	}

	return manifest, payload.Bytes(), nil
}

// solidBlockCache lazily decompresses ONLY the exact layer solid block(s) needed (or legacy block 0)!
type solidBlockCache struct {
	blocks map[int][]byte
}

func newSolidBlockCache() *solidBlockCache {
	return &solidBlockCache{blocks: make(map[int][]byte)}
}

func (c *solidBlockCache) get(manifest *ContainerManifest, payload []byte, blockID int, dec *zstd.Decoder) ([]byte, error) {
	if raw, ok := c.blocks[blockID]; ok {
		return raw, nil
	}
	if blockID == 0 {
		if !manifest.SolidCompressed {
			return nil, fmt.Errorf("bloco sólido legado ausente no manifesto")
		}
		if manifest.SolidOffset < 0 || manifest.SolidOffset+manifest.SolidStoredSize > int64(len(payload)) {
			return nil, fmt.Errorf("bloco sólido truncado no contêiner")
		}
		raw, err := dec.DecodeAll(payload[manifest.SolidOffset:manifest.SolidOffset+manifest.SolidStoredSize], nil)
		if err != nil {
			return nil, fmt.Errorf("erro descomprimindo bloco sólido zstd: %w", err)
		}
		c.blocks[0] = raw
		return raw, nil
	}
	for _, blk := range manifest.SolidBlocks {
		if blk.ID == blockID {
			if blk.Offset < 0 || blk.Offset+blk.StoredSize > int64(len(payload)) {
				return nil, fmt.Errorf("bloco sólido da camada %d (ID %d) truncado no contêiner", blk.LayerDepth, blk.ID)
			}
			raw, err := dec.DecodeAll(payload[blk.Offset:blk.Offset+blk.StoredSize], nil)
			if err != nil {
				return nil, fmt.Errorf("erro descomprimindo bloco sólido da camada %d: %w", blk.LayerDepth, err)
			}
			c.blocks[blockID] = raw
			return raw, nil
		}
	}
	return nil, fmt.Errorf("bloco sólido ID %d não encontrado no manifesto", blockID)
}

func resolveEntryBytes(manifest *ContainerManifest, payload []byte, cache *solidBlockCache, f *PackedFileEntry, dec *zstd.Decoder, ffmpegPath string) ([]byte, error) {
	var raw []byte
	var err error
	if f.InSolidBlock {
		solidRaw, sErr := cache.get(manifest, payload, f.SolidBlockID, dec)
		if sErr != nil {
			return nil, sErr
		}
		if f.Offset < 0 || f.Offset+f.StoredSize > int64(len(solidRaw)) {
			return nil, fmt.Errorf("offset sólido inválido para %s", f.RelPath)
		}
		raw = solidRaw[f.Offset : f.Offset+f.StoredSize]
	} else {
		if f.Offset < 0 || f.Offset+f.StoredSize > int64(len(payload)) {
			return nil, fmt.Errorf("offset inválido para %s", f.RelPath)
		}
		stored := payload[f.Offset : f.Offset+f.StoredSize]
		raw = stored
		if f.Compressed {
			raw, err = dec.DecodeAll(stored, nil)
			if err != nil {
				return nil, fmt.Errorf("erro descomprimindo %s: %w", f.RelPath, err)
			}
		}
	}

	switch f.Transform {
	case "delta-pcm16":
		raw, err = deltaDecodePCM16(raw)
		if err != nil {
			return nil, fmt.Errorf("erro revertendo delta-pcm16 em %s: %w", f.RelPath, err)
		}
		if f.SHA256 != "" && crypto.SHA256Hex(raw) != f.SHA256 {
			return nil, fmt.Errorf("integridade SHA-256 falhou para %s", f.RelPath)
		}
	case "webp-lossless":
		if f.StoredSHA256 != "" && crypto.SHA256Hex(raw) != f.StoredSHA256 {
			return nil, fmt.Errorf("integridade WebP Lossless falhou para %s", f.RelPath)
		}
		raw, err = convertWebPLosslessToPNG(ffmpegPath, raw)
		if err != nil {
			return nil, fmt.Errorf("erro restaurando PNG de WebP Lossless em %s: %w", f.RelPath, err)
		}
	default:
		if f.SHA256 != "" && crypto.SHA256Hex(raw) != f.SHA256 {
			return nil, fmt.Errorf("integridade SHA-256 falhou para %s", f.RelPath)
		}
	}
	return raw, nil
}

// newPackerZstdDecoder creates a Zstd decoder pre-loaded with BrightNet/3's WebDictV1
// (seamlessly decoding both standard dict-less Zstd v1/v2/v3 blocks AND WebDictV1 blocks!).
func newPackerZstdDecoder() (*zstd.Decoder, error) {
	return zstd.NewReader(nil, zstd.WithDecoderDictRaw(WebDictV1ID, WebDictV1))
}

var reHTMLAssetRef = regexp.MustCompile(`(?i)(?:href|src)\s*=\s*["']([^"'#?]+)["']`)

func extractCriticalHTMLRefs(htmlText string) map[string]bool {
	matches := reHTMLAssetRef.FindAllStringSubmatch(htmlText, 128)
	if len(matches) == 0 {
		return nil
	}
	refs := make(map[string]bool, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		raw := strings.TrimSpace(m[1])
		low := strings.ToLower(raw)
		if raw == "" || strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") ||
			strings.HasPrefix(low, "//") || strings.HasPrefix(low, "data:") || strings.HasPrefix(low, "javascript:") ||
			strings.HasPrefix(low, "btweb://") || strings.HasPrefix(low, "ytweb://") {
			continue
		}
		clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+raw)), "/")
		if clean != "" {
			refs[strings.ToLower(clean)] = true
		}
	}
	return refs
}

// ExtractFiles extracts all (or only selectedRelPaths if non-empty) files from a decrypted container payload.
// Lazily decompresses ONLY the layer solid blocks required by the requested files!
func ExtractFiles(manifest *ContainerManifest, payload []byte, destDir string, selectedRelPaths map[string]bool, dateOverrides map[string]string, ffmpegPath ...string) ([]string, error) {
	dec, err := newPackerZstdDecoder()
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	ff := ""
	if len(ffmpegPath) > 0 {
		ff = ffmpegPath[0]
	}

	cache := newSolidBlockCache()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}

	var extracted []string
	for i := range manifest.Files {
		f := &manifest.Files[i]
		if len(selectedRelPaths) > 0 && !selectedRelPaths[f.RelPath] {
			continue
		}
		raw, err := resolveEntryBytes(manifest, payload, cache, f, dec, ff)
		if err != nil {
			return nil, err
		}

		// Prevent path traversal
		cleanRel := filepath.Clean(filepath.FromSlash(f.RelPath))
		if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
			cleanRel = filepath.Base(cleanRel)
		}
		outPath := filepath.Join(destDir, cleanRel)
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(outPath, raw, 0o644); err != nil {
			return nil, err
		}

		effDate := f.ModTime
		if dateOverrides != nil {
			if ov, ok := dateOverrides[f.RelPath]; ok && ov != "" {
				effDate = ov
			}
		}
		_ = ApplyExportMetadata(outPath, effDate)
		extracted = append(extracted, outPath)
	}
	return extracted, nil
}

// ExtractSingleBytes extracts a single file from decrypted payload into memory (lazy layer decompression!).
func ExtractSingleBytes(manifest *ContainerManifest, payload []byte, relPath string, ffmpegPath ...string) ([]byte, *PackedFileEntry, error) {
	dec, err := newPackerZstdDecoder()
	if err != nil {
		return nil, nil, err
	}
	defer dec.Close()

	ff := ""
	if len(ffmpegPath) > 0 {
		ff = ffmpegPath[0]
	}

	cache := newSolidBlockCache()
	for i := range manifest.Files {
		f := &manifest.Files[i]
		if f.RelPath != relPath {
			continue
		}
		raw, err := resolveEntryBytes(manifest, payload, cache, f, dec, ff)
		if err != nil {
			return nil, nil, err
		}
		return raw, f, nil
	}
	return nil, nil, fmt.Errorf("arquivo %s não encontrado no contêiner", relPath)
}

// ExtractAllToMemory unpacks all files from a container payload directly into RAM with full SHA-256 verification.
func ExtractAllToMemory(manifest *ContainerManifest, payload []byte, ffmpegPath ...string) (map[string][]byte, error) {
	dec, err := newPackerZstdDecoder()
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	ff := ""
	if len(ffmpegPath) > 0 {
		ff = ffmpegPath[0]
	}

	cache := newSolidBlockCache()
	out := make(map[string][]byte, len(manifest.Files))
	for i := range manifest.Files {
		f := &manifest.Files[i]
		raw, err := resolveEntryBytes(manifest, payload, cache, f, dec, ff)
		if err != nil {
			return nil, err
		}
		cleanRel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(f.RelPath))), "/")
		out[cleanRel] = raw
	}
	return out, nil
}

// ProgressiveUnpacker incrementally extracts and SHA-256-verifies files from a growing
// contiguous payload prefix as frames stream live (Camada 0 -> Camada 1 -> Camada 2 -> ...).
type ProgressiveUnpacker struct {
	manifest   *ContainerManifest
	ffmpegPath string
	dec        *zstd.Decoder
	cache      *solidBlockCache
	extracted  map[string]bool
	blockBounds map[int][2]int64 // blockID -> [offset, end]
}

// NewProgressiveUnpacker initializes a stateful progressive extractor for a decoded ContainerManifest.
func NewProgressiveUnpacker(manifest *ContainerManifest, ffmpegPath string) (*ProgressiveUnpacker, error) {
	dec, err := newPackerZstdDecoder()
	if err != nil {
		return nil, err
	}
	bounds := make(map[int][2]int64, len(manifest.SolidBlocks)+1)
	if manifest.SolidCompressed {
		bounds[0] = [2]int64{manifest.SolidOffset, manifest.SolidOffset + manifest.SolidStoredSize}
	}
	for _, blk := range manifest.SolidBlocks {
		bounds[blk.ID] = [2]int64{blk.Offset, blk.Offset + blk.StoredSize}
	}
	return &ProgressiveUnpacker{
		manifest:    manifest,
		ffmpegPath:  ffmpegPath,
		dec:         dec,
		cache:       newSolidBlockCache(),
		extracted:   make(map[string]bool, len(manifest.Files)),
		blockBounds: bounds,
	}, nil
}

// MarkExtracted marks a manifest file as already satisfied (e.g. from BrightNet/3's SHA-256 CAS cache).
func (u *ProgressiveUnpacker) MarkExtracted(relPath string) {
	if u != nil {
		u.extracted[relPath] = true
	}
}

// FeedSparse implements BrightNet/3 HTTP/3-style Zero Head-of-Line Blocking extraction:
// any file or LayerSolidBlock whose specific frame interval [firstFrame..lastFrame] has arrived
// in `frames` is unlocked immediately, even if an earlier frame failed and is waiting for Cross-Frame RS!
func (u *ProgressiveUnpacker) FeedSparse(frames map[uint32][]byte, startFrameIdx uint32, frameChunkSize int) (map[string][]byte, error) {
	if u == nil || frameChunkSize <= 0 || len(frames) == 0 {
		return nil, nil
	}
	cs := int64(frameChunkSize)
	readRange := func(off, sz int64) ([]byte, bool) {
		if off < 0 || sz < 0 {
			return nil, false
		}
		if sz == 0 {
			return []byte{}, true
		}
		firstF := int(off / cs)
		lastF := int((off + sz - 1) / cs)
		out := make([]byte, sz)
		written := int64(0)
		for fi := firstF; fi <= lastF; fi++ {
			chunk, ok := frames[startFrameIdx+uint32(fi)]
			if !ok {
				return nil, false
			}
			fStart := int64(fi) * cs
			sliceStart := int64(0)
			if off > fStart {
				sliceStart = off - fStart
			}
			need := sz - written
			avail := int64(len(chunk)) - sliceStart
			if avail <= 0 {
				return nil, false
			}
			take := need
			if avail < take {
				take = avail
			}
			copy(out[written:written+take], chunk[sliceStart:sliceStart+take])
			written += take
		}
		if written != sz {
			return nil, false
		}
		return out, true
	}

	var newlyUnlocked map[string][]byte
	for i := range u.manifest.Files {
		f := &u.manifest.Files[i]
		if u.extracted[f.RelPath] {
			continue
		}
		var raw []byte
		var err error
		if f.InSolidBlock {
			solidRaw, ok := u.cache.blocks[f.SolidBlockID]
			if !ok {
				b, hasB := u.blockBounds[f.SolidBlockID]
				if !hasB {
					continue
				}
				compSlice, ready := readRange(b[0], b[1]-b[0])
				if !ready {
					continue
				}
				solidRaw, err = u.dec.DecodeAll(compSlice, nil)
				if err != nil {
					return newlyUnlocked, err
				}
				u.cache.blocks[f.SolidBlockID] = solidRaw
			}
			if f.Offset < 0 || f.Offset+f.StoredSize > int64(len(solidRaw)) {
				return newlyUnlocked, fmt.Errorf("offset sólido inválido para %s", f.RelPath)
			}
			raw = solidRaw[f.Offset : f.Offset+f.StoredSize]
		} else {
			stored, ready := readRange(f.Offset, f.StoredSize)
			if !ready {
				continue
			}
			raw = stored
			if f.Compressed {
				raw, err = u.dec.DecodeAll(stored, nil)
				if err != nil {
					return newlyUnlocked, fmt.Errorf("erro descomprimindo %s: %w", f.RelPath, err)
				}
			}
		}

		// Apply reversible transforms & strict SHA-256 verification
		switch f.Transform {
		case "delta-pcm16":
			raw, err = deltaDecodePCM16(raw)
			if err != nil {
				return newlyUnlocked, fmt.Errorf("erro revertendo delta-pcm16 em %s: %w", f.RelPath, err)
			}
			if f.SHA256 != "" && crypto.SHA256Hex(raw) != f.SHA256 {
				return newlyUnlocked, fmt.Errorf("integridade SHA-256 falhou para %s", f.RelPath)
			}
		case "webp-lossless":
			if f.StoredSHA256 != "" && crypto.SHA256Hex(raw) != f.StoredSHA256 {
				return newlyUnlocked, fmt.Errorf("integridade WebP Lossless falhou para %s", f.RelPath)
			}
			raw, err = convertWebPLosslessToPNG(u.ffmpegPath, raw)
			if err != nil {
				return newlyUnlocked, fmt.Errorf("erro restaurando PNG de WebP Lossless em %s: %w", f.RelPath, err)
			}
		default:
			if f.SHA256 != "" && crypto.SHA256Hex(raw) != f.SHA256 {
				return newlyUnlocked, fmt.Errorf("integridade SHA-256 falhou para %s", f.RelPath)
			}
		}

		u.extracted[f.RelPath] = true
		if newlyUnlocked == nil {
			newlyUnlocked = make(map[string][]byte)
		}
		cleanRel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(f.RelPath))), "/")
		newlyUnlocked[cleanRel] = raw
	}
	return newlyUnlocked, nil
}

// Close releases the underlying Zstd decoder resources.
func (u *ProgressiveUnpacker) Close() {
	if u != nil && u.dec != nil {
		u.dec.Close()
		u.dec = nil
	}
}

// Feed inspects a growing contiguous payload prefix and extracts every file whose bytes
// (or enclosing LayerSolidBlock) have fully arrived, returning newly unlocked normalized files.
func (u *ProgressiveUnpacker) Feed(contiguousPayload []byte) (map[string][]byte, error) {
	avail := int64(len(contiguousPayload))
	var newlyUnlocked map[string][]byte
	for i := range u.manifest.Files {
		f := &u.manifest.Files[i]
		if u.extracted[f.RelPath] {
			continue
		}
		if f.InSolidBlock {
			b, ok := u.blockBounds[f.SolidBlockID]
			if !ok || b[0] < 0 || b[1] > avail {
				continue
			}
		} else {
			if f.Offset < 0 || f.Offset+f.StoredSize > avail {
				continue
			}
		}
		raw, err := resolveEntryBytes(u.manifest, contiguousPayload, u.cache, f, u.dec, u.ffmpegPath)
		if err != nil {
			return newlyUnlocked, err
		}
		u.extracted[f.RelPath] = true
		if newlyUnlocked == nil {
			newlyUnlocked = make(map[string][]byte)
		}
		cleanRel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(f.RelPath))), "/")
		newlyUnlocked[cleanRel] = raw
	}
	return newlyUnlocked, nil
}

// ExtractedCount returns how many files have been unlocked and verified so far.
func (u *ProgressiveUnpacker) ExtractedCount() int {
	if u == nil {
		return 0
	}
	return len(u.extracted)
}

func MarshalManifest(m *ContainerManifest) ([]byte, error) {
	return json.Marshal(m)
}

func UnmarshalManifest(b []byte) (*ContainerManifest, error) {
	var m ContainerManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// GeneratePreview creates a compact preview (JPEG thumbnail or text snippet) for the pre-download viewer.
func GeneratePreview(raw []byte, localPath, ext, mimeType, ffmpegPath string) (string, string) {
	// 1. Images (JPEG, PNG, GIF) -> resize to max 360px JPEG (~8-20 KB)
	if strings.HasPrefix(mimeType, "image/") || ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" {
		if img, _, err := image.Decode(bytes.NewReader(raw)); err == nil {
			thumb := resizeBilinear(img, 360)
			var buf bytes.Buffer
			if jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 72}) == nil {
				return "image", base64.StdEncoding.EncodeToString(buf.Bytes())
			}
		}
	}

	// 2. Videos (when FFmpeg + localPath available) -> 2x2 storyboard contact sheet JPEG
	if (strings.HasPrefix(mimeType, "video/") || ext == ".mp4" || ext == ".mkv" || ext == ".mov" || ext == ".webm") && localPath != "" && ffmpegPath != "" {
		cmd := exec.Command(ffmpegPath,
			"-loglevel", "error",
			"-i", localPath,
			"-vf", "fps=1/5,scale=240:-1,tile=2x2",
			"-frames:v", "1",
			"-f", "image2pipe",
			"-vcodec", "mjpeg",
			"pipe:1",
		)
		sysutil.ApplySubprocessAttrs(cmd, true)
		if out, err := cmd.Output(); err == nil && len(out) > 200 && len(out) < 80*1024 {
			return "video", base64.StdEncoding.EncodeToString(out)
		}
	}

	// 3. Text / Code / Config / CSV -> first 2 KB UTF-8 snippet
	textExts := map[string]bool{
		".txt": true, ".md": true, ".json": true, ".csv": true, ".xml": true,
		".yaml": true, ".yml": true, ".ini": true, ".log": true, ".go": true,
		".py": true, ".js": true, ".ts": true, ".html": true, ".css": true,
		".sql": true, ".sh": true, ".bat": true, ".ps1": true,
	}
	if strings.HasPrefix(mimeType, "text/") || textExts[ext] {
		limit := len(raw)
		if limit > 2048 {
			limit = 2048
		}
		snip := raw[:limit]
		if utf8.Valid(snip) {
			return "text", base64.StdEncoding.EncodeToString(snip)
		}
	}

	return "none", ""
}

func resizeBilinear(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return src
	}
	dw, dh := sw, sh
	if sw > maxDim || sh > maxDim {
		if sw >= sh {
			dw = maxDim
			dh = int(float64(sh) * float64(maxDim) / float64(sw))
		} else {
			dh = maxDim
			dw = int(float64(sw) * float64(maxDim) / float64(sh))
		}
	}
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	xRatio := float64(sw) / float64(dw)
	yRatio := float64(sh) / float64(dh)
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + int(float64(y)*yRatio)
		if sy >= b.Max.Y {
			sy = b.Max.Y - 1
		}
		for x := 0; x < dw; x++ {
			sx := b.Min.X + int(float64(x)*xRatio)
			if sx >= b.Max.X {
				sx = b.Max.X - 1
			}
			r, g, bl, a := src.At(sx, sy).RGBA()
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r >> 8),
				G: uint8(g >> 8),
				B: uint8(bl >> 8),
				A: uint8(a >> 8),
			})
		}
	}
	return dst
}

// CopyReaderToTemp saves an io.Reader (e.g. multipart upload) to a temp file safely.
func CopyReaderToTemp(r io.Reader, dir, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, name)
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return "", err
	}
	return dst, nil
}

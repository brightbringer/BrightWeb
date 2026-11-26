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
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"ytcloud/internal/crypto"
	"ytcloud/internal/sysutil"
)

// PackedFileEntry describes one file inside a YTCloud video container manifest.
type PackedFileEntry struct {
	RelPath       string `json:"rel_path"`
	Name          string `json:"name"`
	MimeType      string `json:"mime_type"`
	OriginalSize  int64  `json:"original_size"`
	StoredSize    int64  `json:"stored_size"`
	Compressed    bool   `json:"compressed"`
	InSolidBlock  bool   `json:"in_solid_block,omitempty"` // if true, Offset is inside the decompressed Solid block
	Transform     string `json:"transform,omitempty"`      // "", "delta-pcm16", "webp-lossless"
	Offset        int64  `json:"offset"`                   // byte offset in container payload blob (or inside Solid block)
	SHA256        string `json:"sha256"`
	StoredSHA256  string `json:"stored_sha256,omitempty"` // integrity hash for pixel-exact transforms like webp-lossless
	ModTime       string `json:"mod_time"`
	PreviewKind   string `json:"preview_kind,omitempty"`   // "image", "text", "video", "none"
	PreviewBase64 string `json:"preview_base64,omitempty"` // compact JPEG thumbnail or text snippet
	LocalPath     string `json:"-"`                        // not serialized inside YouTube manifest
}

// ContainerManifest is embedded in the Manifest-First frames (first 2-5s of every video).
type ContainerManifest struct {
	Version         int               `json:"version"`
	ContainerID     string            `json:"container_id"`
	CreatedAt       string            `json:"created_at"`
	ProfileCode     string            `json:"profile_code"`
	Resolution      string            `json:"resolution"`
	TotalSize       int64             `json:"total_size"`
	SolidCompressed bool              `json:"solid_compressed,omitempty"`
	SolidOffset     int64             `json:"solid_offset,omitempty"`
	SolidStoredSize int64             `json:"solid_stored_size,omitempty"`
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

	// Group solid-eligible files sorted by (ext, relPath) to maximize cross-file zstd dictionary matches!
	var solidOrder []int
	for i := range items {
		if items[i].wantsSolid {
			solidOrder = append(solidOrder, i)
		}
	}
	sort.Slice(solidOrder, func(a, b int) bool {
		ia, ib := solidOrder[a], solidOrder[b]
		if items[ia].ext != items[ib].ext {
			return items[ia].ext < items[ib].ext
		}
		return items[ia].entry.RelPath < items[ib].entry.RelPath
	})

	manifest := &ContainerManifest{
		Version:     2,
		ContainerID: opts.ContainerID,
		ProfileCode: opts.ProfileCode,
		Resolution:  opts.ResolutionCode,
		Files:       make([]PackedFileEntry, len(items)),
	}

	var payload bytes.Buffer

	const maxSolidRawBytes = 64 * 1024 * 1024 // Cap solid block at 64 MB to keep RAM low & compression fast

	if len(solidOrder) > 0 {
		var solidTotal int64
		for _, idx := range solidOrder {
			solidTotal += int64(len(items[idx].data))
		}

		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderConcurrency(2))
		if err != nil {
			return nil, nil, err
		}

		if solidTotal <= maxSolidRawBytes {
			var solidRaw bytes.Buffer
			solidRaw.Grow(int(solidTotal))
			solidOffsets := make([]int64, len(solidOrder))
			for k, idx := range solidOrder {
				solidOffsets[k] = int64(solidRaw.Len())
				solidRaw.Write(items[idx].data)
			}

			solidComp := enc.EncodeAll(solidRaw.Bytes(), nil)
			enc.Close()

			if len(solidComp) < solidRaw.Len()-64 {
				manifest.SolidCompressed = true
				manifest.SolidOffset = int64(payload.Len())
				manifest.SolidStoredSize = int64(len(solidComp))
				payload.Write(solidComp)
				for k, idx := range solidOrder {
					items[idx].entry.InSolidBlock = true
					items[idx].entry.Compressed = true
					items[idx].entry.Offset = solidOffsets[k]
					items[idx].data = nil
				}
			} else {
				// Solid compression didn't save space; fall back to direct raw storage (undo delta-pcm16 if any!)
				for _, idx := range solidOrder {
					items[idx].wantsSolid = false
					if items[idx].entry.Transform == "delta-pcm16" {
						if orig, err := deltaDecodePCM16(items[idx].data); err == nil {
							items[idx].data = orig
							items[idx].entry.Transform = ""
							items[idx].entry.StoredSize = int64(len(orig))
						}
					}
				}
			}
			solidRaw.Reset()
		} else {
			// Large folder (>64 MB compressible): compress per-file with SpeedBetterCompression to stay within 512 MB RAM!
			for _, idx := range solidOrder {
				comp := enc.EncodeAll(items[idx].data, nil)
				if len(comp) < len(items[idx].data)-64 {
					items[idx].data = comp
					items[idx].entry.Compressed = true
					items[idx].entry.StoredSize = int64(len(comp))
				} else if items[idx].entry.Transform == "delta-pcm16" {
					if orig, err := deltaDecodePCM16(items[idx].data); err == nil {
						items[idx].data = orig
						items[idx].entry.Transform = ""
						items[idx].entry.StoredSize = int64(len(orig))
					}
				}
			}
			enc.Close()
		}
	}

	// Write all non-solid items sequentially into payload
	for i := range items {
		if !items[i].entry.InSolidBlock {
			items[i].entry.Offset = int64(payload.Len())
			payload.Write(items[i].data)
			items[i].data = nil
		}
		manifest.TotalSize += items[i].entry.OriginalSize
		manifest.Files[i] = items[i].entry
	}

	return manifest, payload.Bytes(), nil
}

func resolveEntryBytes(manifest *ContainerManifest, payload []byte, solidRaw []byte, f *PackedFileEntry, dec *zstd.Decoder, ffmpegPath string) ([]byte, error) {
	var raw []byte
	var err error
	if f.InSolidBlock {
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

// ExtractFiles extracts all (or only selectedRelPaths if non-empty) files from a decrypted container payload.
// Returns the list of extracted destination paths.
func ExtractFiles(manifest *ContainerManifest, payload []byte, destDir string, selectedRelPaths map[string]bool, dateOverrides map[string]string, ffmpegPath ...string) ([]string, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	ff := ""
	if len(ffmpegPath) > 0 {
		ff = ffmpegPath[0]
	}

	var solidRaw []byte
	if manifest.SolidCompressed {
		if manifest.SolidOffset < 0 || manifest.SolidOffset+manifest.SolidStoredSize > int64(len(payload)) {
			return nil, fmt.Errorf("bloco sólido truncado no contêiner")
		}
		solidRaw, err = dec.DecodeAll(payload[manifest.SolidOffset:manifest.SolidOffset+manifest.SolidStoredSize], nil)
		if err != nil {
			return nil, fmt.Errorf("erro descomprimindo bloco sólido zstd: %w", err)
		}
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}

	var extracted []string
	for i := range manifest.Files {
		f := &manifest.Files[i]
		if len(selectedRelPaths) > 0 && !selectedRelPaths[f.RelPath] {
			continue
		}
		raw, err := resolveEntryBytes(manifest, payload, solidRaw, f, dec, ff)
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

// ExtractSingleBytes extracts a single file from decrypted payload into memory.
func ExtractSingleBytes(manifest *ContainerManifest, payload []byte, relPath string, ffmpegPath ...string) ([]byte, *PackedFileEntry, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, nil, err
	}
	defer dec.Close()

	ff := ""
	if len(ffmpegPath) > 0 {
		ff = ffmpegPath[0]
	}

	var solidRaw []byte
	if manifest.SolidCompressed {
		if manifest.SolidOffset < 0 || manifest.SolidOffset+manifest.SolidStoredSize > int64(len(payload)) {
			return nil, nil, fmt.Errorf("bloco sólido truncado no contêiner")
		}
		solidRaw, err = dec.DecodeAll(payload[manifest.SolidOffset:manifest.SolidOffset+manifest.SolidStoredSize], nil)
		if err != nil {
			return nil, nil, err
		}
	}

	for i := range manifest.Files {
		f := &manifest.Files[i]
		if f.RelPath != relPath {
			continue
		}
		raw, err := resolveEntryBytes(manifest, payload, solidRaw, f, dec, ff)
		if err != nil {
			return nil, nil, err
		}
		return raw, f, nil
	}
	return nil, nil, fmt.Errorf("arquivo %s não encontrado no contêiner", relPath)
}

// ExtractAllToMemory unpacks all files from a container payload directly into RAM with full SHA-256 verification.
func ExtractAllToMemory(manifest *ContainerManifest, payload []byte, ffmpegPath ...string) (map[string][]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	ff := ""
	if len(ffmpegPath) > 0 {
		ff = ffmpegPath[0]
	}

	var solidRaw []byte
	if manifest.SolidCompressed {
		if manifest.SolidOffset < 0 || manifest.SolidOffset+manifest.SolidStoredSize > int64(len(payload)) {
			return nil, fmt.Errorf("bloco sólido truncado no contêiner")
		}
		solidRaw, err = dec.DecodeAll(payload[manifest.SolidOffset:manifest.SolidOffset+manifest.SolidStoredSize], nil)
		if err != nil {
			return nil, fmt.Errorf("erro descomprimindo bloco sólido zstd: %w", err)
		}
	}

	out := make(map[string][]byte, len(manifest.Files))
	for i := range manifest.Files {
		f := &manifest.Files[i]
		raw, err := resolveEntryBytes(manifest, payload, solidRaw, f, dec, ff)
		if err != nil {
			return nil, err
		}
		cleanRel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(f.RelPath))), "/")
		out[cleanRel] = raw
	}
	return out, nil
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

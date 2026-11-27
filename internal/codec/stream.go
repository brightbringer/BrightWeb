package codec

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"ytcloud/internal/sysutil"
)

var (
	encCheckMu    sync.Mutex
	encCheckCache = map[string]string{}
)

func ffmpegEncodersText(ffmpegPath string) string {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	encCheckMu.Lock()
	if txt, seen := encCheckCache[ffmpegPath]; seen {
		encCheckMu.Unlock()
		return txt
	}
	encCheckMu.Unlock()

	cmd := exec.Command(ffmpegPath, "-hide_banner", "-encoders")
	sysutil.ApplySubprocessAttrs(cmd, true)
	out, err := cmd.Output()
	txt := ""
	if err == nil {
		txt = string(out)
	}
	encCheckMu.Lock()
	encCheckCache[ffmpegPath] = txt
	encCheckMu.Unlock()
	return txt
}

// FFmpegSupportsAV1 checks (with process-lifetime caching) whether libaom-av1 is available.
func FFmpegSupportsAV1(ffmpegPath string) bool {
	return strings.Contains(ffmpegEncodersText(ffmpegPath), "libaom-av1")
}

func ffmpegHasEncoder(ffmpegPath, encName string) bool {
	return strings.Contains(ffmpegEncodersText(ffmpegPath), encName)
}

// ResolveVideoCodec resolves "auto" / "av1" / "av1-mp4" / "av1-mkv" / "h264" / "h264-mp4" / "vp9-webm" / "hevc-mp4"
// into a concrete normalized codec code: "av1", "vp9", "hevc", or "h264".
func ResolveVideoCodec(requested string, prof Profile, ffmpegPath string) string {
	req := strings.ToLower(strings.TrimSpace(requested))
	switch req {
	case "av1", "av1-mp4", "av1-mkv":
		if ffmpegHasEncoder(ffmpegPath, "libaom-av1") {
			return "av1"
		}
		return "h264"
	case "vp9", "vp9-webm":
		if ffmpegHasEncoder(ffmpegPath, "libvpx-vp9") {
			return "vp9"
		}
		return "h264"
	case "hevc", "hevc-mp4", "h265":
		if ffmpegHasEncoder(ffmpegPath, "libx265") {
			return "hevc"
		}
		return "h264"
	case "h264", "h264-mp4", "x264":
		return "h264"
	default: // "" or "auto"
		useAV1Auto := (prof.ArtMode == "" && !prof.UseChroma && prof.YBlockSize == 8) ||
			prof.ArtMode == "chroma-art" || prof.ArtMode == "art-crystal" || prof.ArtMode == "art-fractal3d" || prof.ArtMode == "art-angles"
		if useAV1Auto && ffmpegHasEncoder(ffmpegPath, "libaom-av1") {
			return "av1"
		}
		return "h264"
	}
}

// OutputExtensionForCodec returns the optimal container file extension (".mp4", ".mkv", or ".webm").
func OutputExtensionForCodec(requested string, prof Profile, ffmpegPath string) string {
	req := strings.ToLower(strings.TrimSpace(requested))
	resolved := ResolveVideoCodec(req, prof, ffmpegPath)
	if req == "av1-mkv" && resolved == "av1" {
		return ".mkv"
	}
	if (req == "vp9" || req == "vp9-webm") && resolved == "vp9" {
		return ".webm"
	}
	return ".mp4"
}

func optimalAV1CRF(prof Profile) int {
	switch prof.Code {
	case "y8-gray":
		return 42 // Empirically calibrated valley: 3.51x upload, 0.75s encode, 0 B VP9 good
	case "y6-gray", "y4-gray", "y2-safe", "art-chroma":
		return 46 // Empirically calibrated valley: 3.49x-3.55x (5.00x art-chroma), 0-6 B VP9 good
	case "art-crystal":
		return 44 // Empirically calibrated: 6.89x upload, 7.07x VP9 good, 0 B YT H.264
	case "art-fractal3d", "art-angles", "art-fullres":
		return 40 // Empirically calibrated: 7.61x (fractal3d) / 8.8x-10.7x (fullres) / 11.16x (angles -> 9.42x VP9)
	default:
		return 38
	}
}

func optimalVP9CRF(prof Profile) int {
	switch prof.Code {
	case "y8-gray":
		return 36
	case "y6-gray":
		return 40
	case "y4-gray", "y2-safe":
		return 44
	default:
		return 34
	}
}

const pipeChunkSize = 256 * 1024 // 256 KB chunks prevent Windows ERROR_NO_SYSTEM_RESOURCES on 47MB 8K frames!

func writeChunked(w io.Writer, buf []byte) error {
	for off := 0; off < len(buf); off += pipeChunkSize {
		end := off + pipeChunkSize
		if end > len(buf) {
			end = len(buf)
		}
		if _, err := w.Write(buf[off:end]); err != nil {
			return err
		}
	}
	return nil
}

func readFullChunked(r io.Reader, buf []byte) error {
	for off := 0; off < len(buf); off += pipeChunkSize {
		end := off + pipeChunkSize
		if end > len(buf) {
			end = len(buf)
		}
		if _, err := io.ReadFull(r, buf[off:end]); err != nil {
			return err
		}
	}
	return nil
}

// VideoEncodeOptions controls how encrypted manifest + payload are streamed into a video via FFmpeg.
type VideoEncodeOptions struct {
	Ctx            context.Context // Optional cancellation context (kills FFmpeg cleanly & removes partial file!)
	FFmpegPath     string
	OutputPath     string
	ResolutionCode string // "2k", "4k", "1080p", "720p"
	ProfileCode    string // "y8-gray", "y6-gray", "y4-gray", "art-stego", "video-fractal", "video-chroma", etc.
	VideoCodec     string // "auto", "av1-mp4", "av1-mkv", "h264-mp4", "vp9-webm", "hevc-mp4"
	FPS            int    // default 15
	CRF            int    // 0 = auto optimal CRF per profile & codec
	Threads        int    // CPU thread limit for FFmpeg (default 2 in background mode)
	LowPriority    bool   // run FFmpeg at BELOW_NORMAL priority so PC stays responsive
	ContainerID    uint64
	ArtTheme       uint8  // 0=Auto, 1=Aurora, 2=Waves, 3=Lightning, 4=Vortex, 5=Beach, 6=Flora, 7=Fractal
	MaskIntensity  uint8  // 0=Balanced(1x), 1=Subtle(0.25x), 2=Strong(2x), 3=Max(4x)
	HideHeaderBar  bool   // Embed header seamlessly inside art/chroma (zero B&W top bar!)
	HostVideoPath  string // Optional local host video file for blind chroma/fractal carrier profiles (auto-looped)
	AudioMode      string       // ""/"auto", "none", "ambient", "host", "file"
	AudioPath      string       // Optional local audio file when AudioMode == "file"
	MusicOpts      MusicOptions // Optional generative music studio settings (style, mood, instruments)
	TargetFrames   int          // Optional mandatory minimum frame count (e.g. from host video or audio duration)
	PaddingMode    string       // ""/"auto" (auto-soften bits + continuous texture), "spread" (spread data across whole video with stride), "start" (data at start, clean tail)
	CrossFECMode   string       // ""/"auto" (auto K=6,M=2 on Stealth/Art >=24 frames), "high" (K=8,M=2), "max" (K=6,M=2), "off"
	OnProgress     func(frame, total int)
}

// VideoEncodeStats summarizes the generated video container.
type VideoEncodeStats struct {
	OutputPath             string `json:"output_path"`
	Width                  int    `json:"width"`
	Height                 int    `json:"height"`
	FPS                    int    `json:"fps"`
	ProfileCode            string `json:"profile_code"`
	VideoCodec             string `json:"video_codec"`
	ManifestFrames         int    `json:"manifest_frames"`
	DataFrames             int    `json:"data_frames"`
	CrossFrameParityFrames int    `json:"cross_frame_parity_frames,omitempty"`
	CrossFrameK            int    `json:"cross_frame_k,omitempty"`
	CrossFrameM            int    `json:"cross_frame_m,omitempty"`
	PartitionedEncode      bool   `json:"partitioned_encode,omitempty"`
	TotalFrames            int    `json:"total_frames"`
	VideoSizeBytes         int64  `json:"video_size_bytes"`
	PayloadPerFrame        int    `json:"payload_per_frame"`
}

func isStaticImagePath(p string) bool {
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(p))) {
	case ".jpg", ".jpeg", ".jfif", ".png", ".webp", ".bmp", ".tif", ".tiff", ".avif", ".heic", ".heif":
		return true
	default:
		return false
	}
}

func isNoAudioMediaPath(p string) bool {
	if isStaticImagePath(p) {
		return true
	}
	return strings.ToLower(filepath.Ext(strings.TrimSpace(p))) == ".gif"
}

// loadStaticHostImage decodes a single static image (.jpg/.png/.webp/etc.) ONCE into a YUV420p frame
// scaled & center-cropped to (w, h). Avoids FFmpeg -stream_loop -1 deadlocks on the image2 demuxer!
func loadStaticHostImage(ffmpeg, imgPath string, w, h int, lowPriority bool) (*YUVFrame, error) {
	cmd := exec.Command(ffmpeg,
		"-loglevel", "error",
		"-i", imgPath,
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1", w, h, w, h),
		"-frames:v", "1",
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"pipe:1",
	)
	sysutil.ApplySubprocessAttrs(cmd, lowPriority)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("erro lendo imagem base (%s): %w (%s)", filepath.Base(imgPath), err, strings.TrimSpace(stderr.String()))
	}
	fr := NewYUVFrame(w, h)
	if len(out) < len(fr.Buf) {
		return nil, fmt.Errorf("imagem base retornou menos bytes que o esperado (%d < %d)", len(out), len(fr.Buf))
	}
	copy(fr.Buf, out[:len(fr.Buf)])
	return fr, nil
}

// EncodeStreamToVideo streams Manifest frames first (Manifest-First Layout), followed by encrypted
// payload frames directly into FFmpeg rawvideo stdin without writing intermediate PNGs to disk.
func EncodeStreamToVideo(opts VideoEncodeOptions, encryptedManifest []byte, encryptedPayload []byte) (*VideoEncodeStats, error) {
	res, err := ResolutionByCode(opts.ResolutionCode)
	if err != nil {
		return nil, err
	}
	prof, err := ProfileByCode(opts.ProfileCode)
	if err != nil {
		return nil, err
	}
	fps := opts.FPS
	if fps <= 0 {
		fps = 10
	}
	ffmpeg := opts.FFmpegPath
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	vCodec := ResolveVideoCodec(opts.VideoCodec, prof, ffmpeg)
	crf := opts.CRF
	if crf <= 0 {
		switch vCodec {
		case "av1":
			crf = optimalAV1CRF(prof)
		case "vp9":
			crf = optimalVP9CRF(prof)
		default:
			if prof.DefaultCRF > 0 {
				crf = prof.DefaultCRF
			} else {
				crf = 38
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0o755); err != nil {
		return nil, err
	}

	padMode := strings.ToLower(strings.TrimSpace(opts.PaddingMode))
	if padMode == "" {
		padMode = "auto"
	}

	effMaskIntensity := opts.MaskIntensity
	baseFlags := BuildHeaderFlags(effMaskIntensity, opts.HideHeaderBar)
	hasValidHost := false
	if opts.HostVideoPath != "" {
		if _, err := os.Stat(opts.HostVideoPath); err == nil {
			hasValidHost = true
			if prof.ArtMode == "subtractive-y2" || prof.ArtMode == "video-bw" {
				baseFlags |= FlagHostManchester
			}
		}
	}
	if opts.HideHeaderBar && !CanHideHeaderArtMode(prof.ArtMode, hasValidHost) {
		baseFlags &^= FlagHiddenHeader
	}

	// Smart Auto-Soften: when TargetFrames exceeds needed frames under Balanced intensity and the payload
	// still fits comfortably using gentler, larger Subtle bits (e.g. 16x16), auto-switch to Subtle!
	// Skip for Stealth modes so they keep their calibrated H.264/VP9 amplitude margin.
	if padMode == "auto" && opts.TargetFrames > 0 && effMaskIntensity == MaskIntensityDefault && prof.ArtMode != "" && !IsStealthArtMode(prof.ArtMode) {
		subtleFlags := BuildHeaderFlags(MaskIntensitySubtle, baseFlags&FlagHiddenHeader != 0) | (baseFlags & FlagHostManchester)
		subtleCap := prof.PayloadCapacityLayoutWithFlags(res.Width, res.Height, true, subtleFlags)
		if subtleCap > 0 {
			sMan := (4 + len(encryptedManifest) + subtleCap - 1) / subtleCap
			if sMan == 0 {
				sMan = 1
			}
			sData := 0
			if len(encryptedPayload) > 0 {
				sData = (len(encryptedPayload) + subtleCap - 1) / subtleCap
			}
			if sMan+sData <= opts.TargetFrames {
				effMaskIntensity = MaskIntensitySubtle
				baseFlags = subtleFlags
			}
		}
	}

	capPerFrame := prof.PayloadCapacityLayoutWithFlags(res.Width, res.Height, true, baseFlags)
	if capPerFrame <= 0 {
		return nil, errors.New("capacidade útil do frame inválida")
	}

	// Calculate manifest & data frame counts, plus optional Spread Stride when PaddingMode == "spread"
	manifestBlobLen := 4 + len(encryptedManifest)
	manifestFrames := (manifestBlobLen + capPerFrame - 1) / capPerFrame
	if manifestFrames == 0 {
		manifestFrames = 1
	}
	dataFrames := 0
	if len(encryptedPayload) > 0 {
		dataFrames = (len(encryptedPayload) + capPerFrame - 1) / capPerFrame
	}
	totalFrames := manifestFrames + dataFrames
	outputFrames := totalFrames
	if opts.TargetFrames > outputFrames {
		outputFrames = opts.TargetFrames
	}

	spreadStride := 1
	if padMode == "spread" && outputFrames > totalFrames && dataFrames > 0 {
		// Recompute with +4 bytes stride header in manifestBlob
		mFramesSpread := (8 + len(encryptedManifest) + capPerFrame - 1) / capPerFrame
		if mFramesSpread == 0 {
			mFramesSpread = 1
		}
		avail := outputFrames - mFramesSpread
		if avail >= dataFrames*2 {
			manifestFrames = mFramesSpread
			totalFrames = manifestFrames + dataFrames
			spreadStride = avail / dataFrames
			if spreadStride < 2 {
				spreadStride = 2
			}
		}
	}

	var manifestBlob []byte
	var parityChunks [][]byte
	var cfSpec CrossFrameSpec
	if spreadStride > 1 {
		// [4B mLen][4B spreadStride][encryptedManifest] signaled via FlagSpread on manifest frames
		manifestBlob = make([]byte, 8+len(encryptedManifest))
		binary.LittleEndian.PutUint32(manifestBlob[0:4], uint32(len(encryptedManifest)))
		binary.LittleEndian.PutUint32(manifestBlob[4:8], uint32(spreadStride))
		copy(manifestBlob[8:], encryptedManifest)
	} else {
		// Optional Cross-Frame Reed-Solomon Parity (YTCF) to survive up to 25% YouTube VBR blackouts!
		cfK, cfM := ResolveCrossFrameParams(opts.CrossFECMode, prof, dataFrames, spreadStride)
		if cfK > 0 && cfM > 0 {
			parityChunks, cfSpec = BuildCrossFrameParityChunks(encryptedPayload, capPerFrame, cfK, cfM)
			trailer := cfSpec.MarshalTrailer()
			manifestBlob = make([]byte, 4+len(encryptedManifest)+CrossFrameTrailerSize)
			binary.LittleEndian.PutUint32(manifestBlob[0:4], uint32(len(encryptedManifest)))
			copy(manifestBlob[4:4+len(encryptedManifest)], encryptedManifest)
			copy(manifestBlob[4+len(encryptedManifest):], trailer[:])
			manifestFrames = (len(manifestBlob) + capPerFrame - 1) / capPerFrame
			if manifestFrames == 0 {
				manifestFrames = 1
			}
			totalFrames = manifestFrames + dataFrames + len(parityChunks)
			if totalFrames > outputFrames {
				outputFrames = totalFrames
			}
		} else {
			// Standard [4B mLen][encryptedManifest] (100% backwards-compatible)
			manifestBlob = make([]byte, 4+len(encryptedManifest))
			binary.LittleEndian.PutUint32(manifestBlob[0:4], uint32(len(encryptedManifest)))
			copy(manifestBlob[4:], encryptedManifest)
		}
	}

	threads := opts.Threads
	if threads <= 0 {
		threads = 2
	}
	maxrate := "32M"
	bufsize := "64M"
	if res.Width <= 1280 {
		if fps >= 48 {
			maxrate, bufsize = "24M", "48M"
		} else {
			maxrate, bufsize = "14M", "28M"
		}
	} else if res.Width <= 1920 {
		maxrate, bufsize = "16M", "32M"
	} else if res.Width <= 2560 {
		maxrate, bufsize = "22M", "44M"
	} else if res.Width <= 3840 {
		maxrate, bufsize = "26M", "52M"
	}

	// Resolve optional audio input (ambient synth, host video audio, custom audio file, or silent)
	audioMode := strings.ToLower(strings.TrimSpace(opts.AudioMode))
	if audioMode == "" || audioMode == "auto" {
		if prof.GenerateAudio {
			audioMode = "ambient"
		} else if opts.HostVideoPath != "" && !isNoAudioMediaPath(opts.HostVideoPath) {
			audioMode = "host"
		} else {
			audioMode = "none"
		}
	}
	var audioInputPath string
	var tempAudioPath string
	switch audioMode {
	case "ambient":
		durSec := float64(outputFrames) / float64(fps)
		if durSec < 1.5 {
			durSec = 1.5
		}
		mOpts := opts.MusicOpts
		if mOpts.Seed == 0 {
			mOpts.Seed = opts.ContainerID
		}
		wavBytes := SynthMusicWAV(durSec+0.25, mOpts)
		tempAudioPath = opts.OutputPath + ".ambient.wav"
		if err := os.WriteFile(tempAudioPath, wavBytes, 0o600); err == nil {
			defer os.Remove(tempAudioPath)
			audioInputPath = tempAudioPath
		}
	case "host":
		if opts.HostVideoPath != "" && !isNoAudioMediaPath(opts.HostVideoPath) {
			if _, err := os.Stat(opts.HostVideoPath); err == nil {
				audioInputPath = opts.HostVideoPath
			}
		}
	case "file":
		if opts.AudioPath != "" {
			if _, err := os.Stat(opts.AudioPath); err == nil {
				audioInputPath = opts.AudioPath
			}
		}
	}

	// Build FFmpeg command (streaming raw yuv420p -> AV1 / VP9 / HEVC / H.264)
	args := []string{
		"-y",
		"-loglevel", "error",
		"-threads", strconv.Itoa(threads),
		"-f", "rawvideo",
		"-pixel_format", "yuv420p",
		"-video_size", fmt.Sprintf("%dx%d", res.Width, res.Height),
		"-framerate", strconv.Itoa(fps),
		"-i", "pipe:0",
	}
	ext := strings.ToLower(filepath.Ext(opts.OutputPath))
	exactDurStr := fmt.Sprintf("%.3f", float64(outputFrames)/float64(fps))
	if audioInputPath != "" {
		aCodec := "aac"
		aBitrate := "128k"
		if ext == ".webm" || ext == ".mkv" {
			aCodec = "libopus"
			aBitrate = "96k"
		}
		args = append(args,
			"-stream_loop", "-1", "-i", audioInputPath,
			"-map", "0:v:0", "-map", "1:a:0?",
			"-c:a", aCodec, "-b:a", aBitrate,
			"-t", exactDurStr, "-shortest",
		)
	}
	switch vCodec {
	case "av1":
		// AV1 realtime mode: -30% smaller (~3.5x upload), 0 B VP9 errors, faster than x264 medium!
		args = append(args,
			"-c:v", "libaom-av1",
			"-b:v", "0",
			"-crf", strconv.Itoa(crf),
			"-row-mt", "1",
			"-threads", strconv.Itoa(threads),
			"-cpu-used", "8",
			"-usage", "realtime",
			"-tiles", "2x2",
			"-pix_fmt", "yuv420p",
			"-g", strconv.Itoa(fps),
		)
		if ext == ".mp4" {
			args = append(args, "-movflags", "+faststart")
		}
		args = append(args, opts.OutputPath)
	case "vp9":
		args = append(args,
			"-c:v", "libvpx-vp9",
			"-b:v", "0",
			"-crf", strconv.Itoa(crf),
			"-row-mt", "1",
			"-threads", strconv.Itoa(threads),
			"-cpu-used", "5",
			"-deadline", "good",
			"-pix_fmt", "yuv420p",
			"-g", strconv.Itoa(fps),
			opts.OutputPath,
		)
	case "hevc":
		args = append(args,
			"-c:v", "libx265",
			"-preset", "fast",
			"-crf", strconv.Itoa(crf),
			"-x265-params", "log-level=error:bframes=0:ref=1",
			"-tag:v", "hvc1",
			"-pix_fmt", "yuv420p",
			"-g", strconv.Itoa(fps),
			"-movflags", "+faststart",
			opts.OutputPath,
		)
	default:
		args = append(args,
			"-c:v", "libx264",
			"-preset", "medium",
			"-crf", strconv.Itoa(crf),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
			"-threads", strconv.Itoa(threads),
			"-x264-params", "deblock=0:0:rc-lookahead=8:ref=1:bframes=0",
			"-pix_fmt", "yuv420p",
			"-g", strconv.Itoa(fps), // keyframe every 1s for clean seeking / partial peek
			"-movflags", "+faststart",
			opts.OutputPath,
		)
	}

	// Optional host video or static image reader (static images are decoded ONCE into RAM — instant & zero deadlock!)
	var (
		hostCmd       *exec.Cmd
		hostStdout    io.ReadCloser
		hostBuf       *YUVFrame
		staticHostBuf *YUVFrame
	)
	if opts.HostVideoPath != "" {
		if _, err := os.Stat(opts.HostVideoPath); err == nil {
			if isStaticImagePath(opts.HostVideoPath) {
				if imgFr, err := loadStaticHostImage(ffmpeg, opts.HostVideoPath, res.Width, res.Height, opts.LowPriority); err == nil {
					staticHostBuf = imgFr
				} else {
					return nil, err
				}
			} else {
				hCmd := exec.Command(ffmpeg,
					"-loglevel", "error",
					"-threads", strconv.Itoa(threads),
					"-stream_loop", "-1",
					"-i", opts.HostVideoPath,
					"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,fps=%d", res.Width, res.Height, res.Width, res.Height, fps),
					"-f", "rawvideo",
					"-pix_fmt", "yuv420p",
					"pipe:1",
				)
				sysutil.ApplySubprocessAttrs(hCmd, opts.LowPriority)
				if hs, err := hCmd.StdoutPipe(); err == nil {
					if err := hCmd.Start(); err == nil {
						hostCmd = hCmd
						hostStdout = hs
						hostBuf = NewYUVFrame(res.Width, res.Height)
						defer func() {
							_ = hostCmd.Process.Kill()
							_ = hostCmd.Wait()
						}()
					}
				}
			}
		}
	}

	// Automatic 4x Partitioned Parallel Encode (0, 0.25, 0.50, 0.75 + concat -c copy) for VP9 & AV1
	// when encoding >= 120 frames without a streaming host video pipe (empirically 1.6x-2.5x faster!).
	availWorkers := threads
	if gW := GetMaxWorkers(); gW > availWorkers {
		availWorkers = gW
	}
	if (vCodec == "vp9" || vCodec == "av1") && outputFrames >= 120 && hostStdout == nil && spreadStride <= 1 && availWorkers >= 4 {
		stats, pErr := encodeStreamPartitioned4x(
			ffmpeg, opts, prof, res, vCodec, crf, fps, maxrate, bufsize, availWorkers,
			baseFlags, padMode, capPerFrame, manifestBlob, manifestFrames,
			encryptedPayload, dataFrames, parityChunks, cfSpec, totalFrames, outputFrames,
			staticHostBuf, audioInputPath, ext, exactDurStr,
		)
		if pErr == nil {
			return stats, nil
		}
		// Seamless fallback to single-process encode if concat/partition fails
	}

	if opts.Ctx != nil && opts.Ctx.Err() != nil {
		return nil, opts.Ctx.Err()
	}
	cmd := exec.Command(ffmpeg, args...)
	sysutil.ApplySubprocessAttrs(cmd, opts.LowPriority)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("erro abrindo pipe FFmpeg: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("erro iniciando FFmpeg (%s): %w", ffmpeg, err)
	}
	encDoneCh := make(chan struct{})
	defer close(encDoneCh)
	if opts.Ctx != nil {
		go func() {
			select {
			case <-opts.Ctx.Done():
				_ = stdin.Close()
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				if hostCmd != nil && hostCmd.Process != nil {
					_ = hostCmd.Process.Kill()
				}
			case <-encDoneCh:
			}
		}()
	}

	// Multi-core ordered parallel frame encoding pipeline (respects global CPU/RAM resource budget!)
	encWorkers := opts.Threads
	if encWorkers <= 0 {
		encWorkers = GetMaxWorkers()
	}
	maxEncWorkers := 8
	if res.Width >= 3840 {
		maxEncWorkers = 6
	}
	if ramMB, _ := GetResourceBudget(); ramMB > 0 {
		frameMB := float64(res.Width*res.Height*3/2) / (1024 * 1024)
		// Keep frame buffer pool within ~35% of configured RAM budget (leaving room for Go heap & FFmpeg)
		ramCap := int(float64(ramMB)*0.35/frameMB) - 2
		if ramCap < 1 {
			ramCap = 1
		}
		if encWorkers > ramCap {
			encWorkers = ramCap
		}
	}
	if encWorkers > maxEncWorkers {
		encWorkers = maxEncWorkers
	}
	if encWorkers < 1 || outputFrames <= 2 {
		encWorkers = 1
	}

	type encodeResult struct {
		outBuf *YUVFrame
		err    error
	}
	type encodeJob struct {
		outBuf      *YUVFrame
		hostBuf     *YUVFrame
		ownedHost   bool
		cleanVisual bool
		hdr         FrameHeader
		chunk       []byte
		done        chan encodeResult
	}

	bufCap := encWorkers + 2
	freeOut := make(chan *YUVFrame, bufCap)
	for i := 0; i < bufCap; i++ {
		freeOut <- NewYUVFrame(res.Width, res.Height)
	}
	var freeHost chan *YUVFrame
	if hostStdout != nil {
		freeHost = make(chan *YUVFrame, bufCap)
		if hostBuf != nil {
			freeHost <- hostBuf
		}
		for len(freeHost) < bufCap {
			freeHost <- NewYUVFrame(res.Width, res.Height)
		}
	}

	workCh := make(chan encodeJob, bufCap)
	orderCh := make(chan chan encodeResult, bufCap)

	var encWG sync.WaitGroup
	for wi := 0; wi < encWorkers; wi++ {
		encWG.Add(1)
		go func() {
			defer encWG.Done()
			for job := range workCh {
				var err error
				if job.cleanVisual {
					if job.hostBuf != nil {
						copy(job.outBuf.Buf, job.hostBuf.Buf)
						if prof.ArtMode == "video-fractal-chroma" && opts.ArtTheme != ArtThemeNone {
							cb, cr := RenderThemeChromaFields(res.Width/2, res.Height/2, int(job.hdr.FrameIndex), opts.ContainerID, opts.ArtTheme, 48, 208)
							copy(job.outBuf.UPlane(), cb)
							copy(job.outBuf.VPlane(), cr)
						}
					} else if prof.ArtMode != "" {
						RenderProceduralArt(job.outBuf, int(job.hdr.FrameIndex), opts.ContainerID, opts.ArtTheme, 44.0, 212.0, true)
					} else {
						job.outBuf.ClearNeutral()
					}
				} else {
					err = EncodeFrameWithHost(job.outBuf, job.hostBuf, prof, job.hdr, job.chunk)
				}
				if job.ownedHost && freeHost != nil && job.hostBuf != nil {
					freeHost <- job.hostBuf
				}
				job.done <- encodeResult{outBuf: job.outBuf, err: err}
			}
		}()
	}

	var (
		writeErr   error
		writeErrMu sync.Mutex
		writerDone = make(chan struct{})
	)
	getWriteErr := func() error {
		writeErrMu.Lock()
		defer writeErrMu.Unlock()
		return writeErr
	}
	setWriteErr := func(e error) {
		writeErrMu.Lock()
		if writeErr == nil {
			writeErr = e
		}
		writeErrMu.Unlock()
	}

	go func() {
		defer close(writerDone)
		written := 0
		for doneCh := range orderCh {
			res := <-doneCh
			if getWriteErr() == nil {
				if res.err != nil {
					setWriteErr(res.err)
				} else if err := writeChunked(stdin, res.outBuf.Buf); err != nil {
					if opts.Ctx != nil && opts.Ctx.Err() != nil {
						setWriteErr(opts.Ctx.Err())
					} else {
						setWriteErr(fmt.Errorf("erro escrevendo frame %d no FFmpeg: %w", written, err))
					}
				} else {
					written++
					if opts.OnProgress != nil {
						opts.OnProgress(written, outputFrames)
					}
				}
			}
			freeOut <- res.outBuf
		}
	}()

	frameIdx := 0
	acquireHost := func() (*YUVFrame, bool) {
		if staticHostBuf != nil {
			return staticHostBuf, false // immutable shared static image frame — zero copy!
		}
		if hostStdout != nil && freeHost != nil {
			hb := <-freeHost
			if err := readFullChunked(hostStdout, hb.Buf); err == nil {
				return hb, true
			}
			freeHost <- hb
		}
		return nil, false
	}

	submitFrame := func(cleanVisual bool, hdr FrameHeader, chunk []byte) error {
		if opts.Ctx != nil && opts.Ctx.Err() != nil {
			setWriteErr(opts.Ctx.Err())
			return opts.Ctx.Err()
		}
		if err := getWriteErr(); err != nil {
			return err
		}
		outBuf := <-freeOut
		if err := getWriteErr(); err != nil {
			freeOut <- outBuf
			return err
		}
		hb, owned := acquireHost()
		doneCh := make(chan encodeResult, 1)
		orderCh <- doneCh
		workCh <- encodeJob{
			outBuf:      outBuf,
			hostBuf:     hb,
			ownedHost:   owned,
			cleanVisual: cleanVisual,
			hdr:         hdr,
			chunk:       chunk,
			done:        doneCh,
		}
		frameIdx++
		return nil
	}

	writeVisualCleanFrame := func() error {
		hdr := FrameHeader{FrameIndex: uint32(frameIdx)}
		return submitFrame(true, hdr, nil)
	}

	writeChunk := func(chunk []byte, isManifest bool, isLastData bool) error {
		flags := baseFlags
		if isManifest {
			flags |= FlagManifest
		}
		if spreadStride > 1 {
			flags |= FlagSpread
		}
		if isLastData {
			flags |= FlagLastFrame
		}
		hdr := FrameHeader{
			ContainerID: opts.ContainerID,
			ArtTheme:    opts.ArtTheme,
			FrameIndex:  uint32(frameIdx),
			TotalFrames: uint32(totalFrames),
			Flags:       flags,
		}
		return submitFrame(false, hdr, chunk)
	}

	finishPipeline := func() error {
		close(workCh)
		encWG.Wait()
		close(orderCh)
		<-writerDone
		return getWriteErr()
	}

	abortEncode := func(err error) error {
		_ = finishPipeline()
		_ = stdin.Close()
		_ = cmd.Wait()
		if opts.Ctx != nil && opts.Ctx.Err() != nil {
			_ = os.Remove(opts.OutputPath)
			return opts.Ctx.Err()
		}
		return err
	}

	// 1. Write Manifest frames first (always at frames 0..manifestFrames-1 for instant Manifest-First peek!)
	for off := 0; off < len(manifestBlob); off += capPerFrame {
		end := off + capPerFrame
		if end > len(manifestBlob) {
			end = len(manifestBlob)
		}
		if err := writeChunk(manifestBlob[off:end], true, dataFrames == 0 && end >= len(manifestBlob)); err != nil {
			return nil, abortEncode(err)
		}
	}

	// 2. Write Data frames (contiguous OR spread evenly across the video when spreadStride > 1!)
	dataChunkIdx := 0
	for off := 0; off < len(encryptedPayload); off += capPerFrame {
		end := off + capPerFrame
		if end > len(encryptedPayload) {
			end = len(encryptedPayload)
		}
		if spreadStride > 1 && dataChunkIdx > 0 {
			targetPos := manifestFrames + dataChunkIdx*spreadStride
			for frameIdx < targetPos && frameIdx < outputFrames {
				if err := writeVisualCleanFrame(); err != nil {
					return nil, abortEncode(err)
				}
			}
		}
		if err := writeChunk(encryptedPayload[off:end], false, len(parityChunks) == 0 && dataChunkIdx == dataFrames-1); err != nil {
			return nil, abortEncode(err)
		}
		dataChunkIdx++
	}

	// 2b. Write Cross-Frame RS Parity frames (when YTCF shield is active)
	for pi, pChunk := range parityChunks {
		if err := writeChunk(pChunk, false, pi == len(parityChunks)-1); err != nil {
			return nil, abortEncode(err)
		}
	}

	// 3. Optional trailing frames when Mandatory Duration exceeds data frames.
	var emptyFiller []byte
	for frameIdx < outputFrames {
		if padMode == "auto" && prof.ArtMode != "" {
			hdr := FrameHeader{
				ContainerID: opts.ContainerID,
				ArtTheme:    opts.ArtTheme,
				FrameIndex:  uint32(frameIdx),
				TotalFrames: uint32(totalFrames),
				Flags:       baseFlags | FlagVisualFiller,
			}
			if err := submitFrame(false, hdr, emptyFiller); err != nil {
				return nil, abortEncode(err)
			}
		} else {
			if err := writeVisualCleanFrame(); err != nil {
				return nil, abortEncode(err)
			}
		}
	}

	if err := finishPipeline(); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		if opts.Ctx != nil && opts.Ctx.Err() != nil {
			_ = os.Remove(opts.OutputPath)
			return nil, opts.Ctx.Err()
		}
		return nil, err
	}

	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		if opts.Ctx != nil && opts.Ctx.Err() != nil {
			_ = os.Remove(opts.OutputPath)
			return nil, opts.Ctx.Err()
		}
		return nil, fmt.Errorf("FFmpeg encodificação falhou: %v — %s", err, strings.TrimSpace(stderr.String()))
	}
	if opts.Ctx != nil && opts.Ctx.Err() != nil {
		_ = os.Remove(opts.OutputPath)
		return nil, opts.Ctx.Err()
	}

	fi, err := os.Stat(opts.OutputPath)
	var size int64
	if err == nil {
		size = fi.Size()
	}

	return &VideoEncodeStats{
		OutputPath:             opts.OutputPath,
		Width:                  res.Width,
		Height:                 res.Height,
		FPS:                    fps,
		ProfileCode:            prof.Code,
		VideoCodec:             vCodec,
		ManifestFrames:         manifestFrames,
		DataFrames:             dataFrames,
		CrossFrameParityFrames: len(parityChunks),
		CrossFrameK:            cfSpec.K,
		CrossFrameM:            cfSpec.M,
		TotalFrames:            totalFrames,
		VideoSizeBytes:         size,
		PayloadPerFrame:        capPerFrame,
	}, nil
}

// encodeStreamPartitioned4x encodes 4 uniform temporal partitions (0, 0.25, 0.50, 0.75) in parallel
// via 4 FFmpeg instances and stitches them losslessly with `-f concat -c:v copy` (muxing audio in one pass).
func encodeStreamPartitioned4x(
	ffmpeg string, opts VideoEncodeOptions, prof Profile, res Resolution,
	vCodec string, crf, fps int, maxrate, bufsize string, availWorkers int,
	baseFlags uint8, padMode string, capPerFrame int,
	manifestBlob []byte, manifestFrames int,
	encryptedPayload []byte, dataFrames int, parityChunks [][]byte, cfSpec CrossFrameSpec,
	totalFrames, outputFrames int,
	staticHostBuf *YUVFrame, audioInputPath, ext, exactDurStr string,
) (*VideoEncodeStats, error) {
	type frameDesc struct {
		cleanVisual bool
		hdr         FrameHeader
		chunk       []byte
	}
	descs := make([]frameDesc, 0, outputFrames)
	fIdx := 0
	for off := 0; off < len(manifestBlob); off += capPerFrame {
		end := off + capPerFrame
		if end > len(manifestBlob) {
			end = len(manifestBlob)
		}
		flags := baseFlags | FlagManifest
		if dataFrames == 0 && len(parityChunks) == 0 && end >= len(manifestBlob) {
			flags |= FlagLastFrame
		}
		descs = append(descs, frameDesc{
			hdr:   FrameHeader{ContainerID: opts.ContainerID, ArtTheme: opts.ArtTheme, FrameIndex: uint32(fIdx), TotalFrames: uint32(totalFrames), Flags: flags},
			chunk: manifestBlob[off:end],
		})
		fIdx++
	}
	for i := 0; i < dataFrames; i++ {
		off := i * capPerFrame
		end := off + capPerFrame
		if end > len(encryptedPayload) {
			end = len(encryptedPayload)
		}
		flags := baseFlags
		if len(parityChunks) == 0 && i == dataFrames-1 {
			flags |= FlagLastFrame
		}
		descs = append(descs, frameDesc{
			hdr:   FrameHeader{ContainerID: opts.ContainerID, ArtTheme: opts.ArtTheme, FrameIndex: uint32(fIdx), TotalFrames: uint32(totalFrames), Flags: flags},
			chunk: encryptedPayload[off:end],
		})
		fIdx++
	}
	for pi, pChunk := range parityChunks {
		flags := baseFlags
		if pi == len(parityChunks)-1 {
			flags |= FlagLastFrame
		}
		descs = append(descs, frameDesc{
			hdr:   FrameHeader{ContainerID: opts.ContainerID, ArtTheme: opts.ArtTheme, FrameIndex: uint32(fIdx), TotalFrames: uint32(totalFrames), Flags: flags},
			chunk: pChunk,
		})
		fIdx++
	}
	for fIdx < outputFrames {
		if padMode == "auto" && prof.ArtMode != "" {
			descs = append(descs, frameDesc{
				hdr: FrameHeader{ContainerID: opts.ContainerID, ArtTheme: opts.ArtTheme, FrameIndex: uint32(fIdx), TotalFrames: uint32(totalFrames), Flags: baseFlags | FlagVisualFiller},
			})
		} else {
			descs = append(descs, frameDesc{
				cleanVisual: true,
				hdr:         FrameHeader{FrameIndex: uint32(fIdx)},
			})
		}
		fIdx++
	}

	parts := splitUniformRanges(len(descs), 4)
	threadsPerSeg := availWorkers / len(parts)
	if threadsPerSeg < 1 {
		threadsPerSeg = 1
	}
	segExt := ext
	if segExt == "" {
		segExt = ".mp4"
	}
	tmpDir := filepath.Dir(opts.OutputPath)
	baseTmp := filepath.Base(opts.OutputPath)
	segPaths := make([]string, len(parts))
	defer func() {
		for _, sp := range segPaths {
			if sp != "" {
				_ = os.Remove(sp)
			}
		}
	}()

	var (
		wg       sync.WaitGroup
		progMu   sync.Mutex
		doneCnt  int
		errCh    = make(chan error, len(parts))
	)
	for pi, pr := range parts {
		sp := filepath.Join(tmpDir, fmt.Sprintf(".%s.part%d%s", baseTmp, pi, segExt))
		segPaths[pi] = sp
		wg.Add(1)
		go func(idx int, r frameRange, segOut string) {
			defer wg.Done()
			args := []string{
				"-y", "-loglevel", "error",
				"-threads", strconv.Itoa(threadsPerSeg),
				"-f", "rawvideo", "-pixel_format", "yuv420p",
				"-video_size", fmt.Sprintf("%dx%d", res.Width, res.Height),
				"-framerate", strconv.Itoa(fps),
				"-i", "pipe:0",
			}
			if vCodec == "av1" {
				args = append(args,
					"-c:v", "libaom-av1", "-b:v", "0", "-crf", strconv.Itoa(crf),
					"-row-mt", "1", "-threads", strconv.Itoa(threadsPerSeg),
					"-cpu-used", "8", "-usage", "realtime", "-tiles", "2x2",
					"-pix_fmt", "yuv420p", "-g", strconv.Itoa(fps),
					segOut,
				)
			} else {
				args = append(args,
					"-c:v", "libvpx-vp9", "-b:v", "0", "-crf", strconv.Itoa(crf),
					"-row-mt", "1", "-threads", strconv.Itoa(threadsPerSeg),
					"-cpu-used", "5", "-deadline", "good",
					"-pix_fmt", "yuv420p", "-g", strconv.Itoa(fps),
					segOut,
				)
			}
			cmd := exec.Command(ffmpeg, args...)
			if opts.Ctx != nil {
				cmd = exec.CommandContext(opts.Ctx, ffmpeg, args...)
			}
			sysutil.ApplySubprocessAttrs(cmd, opts.LowPriority)
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
			fr := NewYUVFrame(res.Width, res.Height)
			for i := r.start; i < r.end; i++ {
				d := descs[i]
				if d.cleanVisual {
					if staticHostBuf != nil {
						copy(fr.Buf, staticHostBuf.Buf)
					} else if prof.ArtMode != "" {
						RenderProceduralArt(fr, int(d.hdr.FrameIndex), opts.ContainerID, opts.ArtTheme, 44.0, 212.0, true)
					} else {
						fr.ClearNeutral()
					}
				} else if err := EncodeFrameWithHost(fr, staticHostBuf, prof, d.hdr, d.chunk); err != nil {
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
				progMu.Lock()
				doneCnt++
				cur := doneCnt
				progMu.Unlock()
				if opts.OnProgress != nil {
					opts.OnProgress(cur, outputFrames)
				}
			}
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				errCh <- fmt.Errorf("segmento %d falhou: %v (%s)", idx, err, strings.TrimSpace(stderr.String()))
			}
		}(pi, pr, sp)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		return nil, e
	}

	// Stitch all 4 segments losslessly (-c:v copy) and mux optional audio in one single fast pass!
	listPath := filepath.Join(tmpDir, fmt.Sprintf(".%s.concat.txt", baseTmp))
	defer os.Remove(listPath)
	var lb bytes.Buffer
	for _, sp := range segPaths {
		fmt.Fprintf(&lb, "file '%s'\n", strings.ReplaceAll(filepath.ToSlash(sp), "'", "'\\''"))
	}
	if err := os.WriteFile(listPath, lb.Bytes(), 0o600); err != nil {
		return nil, err
	}
	cArgs := []string{"-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", listPath}
	if audioInputPath != "" {
		aCodec := "aac"
		aBitrate := "128k"
		if ext == ".webm" || ext == ".mkv" {
			aCodec = "libopus"
			aBitrate = "96k"
		}
		cArgs = append(cArgs,
			"-stream_loop", "-1", "-i", audioInputPath,
			"-map", "0:v:0", "-map", "1:a:0?",
			"-c:v", "copy", "-c:a", aCodec, "-b:a", aBitrate,
			"-t", exactDurStr, "-shortest",
		)
	} else {
		cArgs = append(cArgs, "-c", "copy")
	}
	if ext == ".mp4" {
		cArgs = append(cArgs, "-movflags", "+faststart")
	}
	cArgs = append(cArgs, opts.OutputPath)
	cCmd := exec.Command(ffmpeg, cArgs...)
	sysutil.ApplySubprocessAttrs(cCmd, opts.LowPriority)
	if out, err := cCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("concat 4x falhou: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	var size int64
	if fi, err := os.Stat(opts.OutputPath); err == nil {
		size = fi.Size()
	}
	return &VideoEncodeStats{
		OutputPath:             opts.OutputPath,
		Width:                  res.Width,
		Height:                 res.Height,
		FPS:                    fps,
		ProfileCode:            prof.Code,
		VideoCodec:             vCodec,
		ManifestFrames:         manifestFrames,
		DataFrames:             dataFrames,
		CrossFrameParityFrames: len(parityChunks),
		CrossFrameK:            cfSpec.K,
		CrossFrameM:            cfSpec.M,
		PartitionedEncode:      true,
		TotalFrames:            totalFrames,
		VideoSizeBytes:         size,
		PayloadPerFrame:        capPerFrame,
	}, nil
}

// ProbeMediaDuration returns the exact duration in seconds of any local video or audio file via ffprobe.
func ProbeMediaDuration(ffprobePath, ffmpegPath, mediaPath string) (float64, error) {
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
	}
	cmd := exec.Command(ffprobePath,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		mediaPath,
	)
	sysutil.ApplySubprocessAttrs(cmd, true)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("erro lendo duração de %s: %w", filepath.Base(mediaPath), err)
	}
	dur, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || dur <= 0 {
		return 0, fmt.Errorf("duração inválida para %s", filepath.Base(mediaPath))
	}
	return dur, nil
}

// ProbeVideoDimensions uses ffprobe (or ffmpeg fallback) to read width & height of a video file.
func ProbeVideoDimensions(ffprobePath, ffmpegPath, videoPath string) (int, int, error) {
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
	}
	cmd := exec.Command(ffprobePath,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "json",
		videoPath,
	)
	sysutil.ApplySubprocessAttrs(cmd, true)
	out, err := cmd.Output()
	if err == nil {
		var parsed struct {
			Streams []struct {
				Width  int `json:"width"`
				Height int `json:"height"`
			} `json:"streams"`
		}
		if json.Unmarshal(out, &parsed) == nil && len(parsed.Streams) > 0 {
			return parsed.Streams[0].Width, parsed.Streams[0].Height, nil
		}
	}
	// Fallback: parse from ffmpeg -i stderr
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	c2 := exec.Command(ffmpegPath, "-i", videoPath)
	sysutil.ApplySubprocessAttrs(c2, true)
	b2, _ := c2.CombinedOutput()
	txt := string(b2)
	for _, res := range Resolutions {
		needle := fmt.Sprintf("%dx%d", res.Width, res.Height)
		if strings.Contains(txt, needle) {
			return res.Width, res.Height, nil
		}
	}
	return 0, 0, fmt.Errorf("não foi possível detectar resolução do vídeo %s", videoPath)
}

// VideoDecodeResult contains the extracted encrypted manifest & payload from a video file.
type VideoDecodeResult struct {
	ContainerID         uint64
	ProfileCode         string
	Width               int
	Height              int
	TotalFrames         int
	DecodedFrames       int
	TotalCorrectedBytes int
	TotalRawBytes       int
	CrossFrameRecovered int
	PartitionedDecode   bool
	EncryptedManifest   []byte
	EncryptedPayload    []byte
}

// DecodeVideoStream decodes an optical video via FFmpeg pipe (automatically engaging 4x parallel
// partitioned decoding on large videos >= 120 frames, Cross-Frame RS reconstruction when YTCF is
// present, and temporal rescue for stealth-film!).
// If manifestOnly is true, stops immediately as soon as all Manifest frames (FlagManifest) are recovered!
// Automatically retries with 8K rescale if YouTube only served a 4K version of an 8K-encoded video.
func DecodeVideoStream(ffmpegPath, ffprobePath, videoPath string, manifestOnly bool, onProgress func(frame, total int)) (*VideoDecodeResult, error) {
	w, h, err := ProbeVideoDimensions(ffprobePath, ffmpegPath, videoPath)
	if err != nil {
		return nil, err
	}
	var durSec float64
	if !manifestOnly {
		durSec, _ = ProbeMediaDuration(ffprobePath, ffmpegPath, videoPath)
	}
	res, err := decodeVideoAtSize(ffmpegPath, videoPath, w, h, durSec, false, manifestOnly, onProgress)
	if err != nil && w == 3840 && h == 2160 {
		// Fallback: video might have been encoded in 8K and downscaled to 4K by YouTube!
		if res8k, err8k := decodeVideoAtSize(ffmpegPath, videoPath, 7680, 4320, durSec, true, manifestOnly, onProgress); err8k == nil {
			return res8k, nil
		}
	}
	return res, err
}

// ProgressiveHooks receives live events while DecodeVideoStreamProgressive streams frames
// from either a direct online video URL (HTTP/HTTPS) or a local video file.
type ProgressiveHooks struct {
	// OnManifest is called as soon as all manifest frames (0..M-1) have decoded.
	// Return false to abort streaming early (e.g. for encrypted, multi-shard, or 100% CAS-cached videos!).
	OnManifest func(encManifest []byte, totalFrames int, profileCode string, width, height int, cfSpec CrossFrameSpec) (continueStream bool)
	// OnContiguous is called whenever the contiguous decoded data prefix grows from frame M forward.
	OnContiguous func(contiguousPayload []byte, decodedFrames, totalFrames int)
	// OnSparseFrame is called whenever ANY data frame arrives (even out-of-order past a missing frame!),
	// powering BrightNet/3 HTTP/3-style Zero Head-of-Line Blocking extraction.
	OnSparseFrame func(seenDataFrames map[uint32][]byte, startDataIdx uint32, frameChunkSize int, decodedFrames, totalFrames int)
	// OnProgress reports frame decoding progress (decodedFrames, totalFrames).
	OnProgress func(decodedFrames, totalFrames int)
}

// DecodeVideoStreamProgressive decodes an optical video sequentially from frame 0 forward
// (supporting direct HTTP/HTTPS streams with auto-reconnect and zero disk write!),
// firing ProgressiveHooks live so BrightWeb can mount Camada 0 (index.html/CSS/JS) immediately
// while Camadas 1..N stream in the background. Multi-core workers decode frames in parallel,
// and Cross-Frame RS transparently repairs any damaged frames upon completion.
func DecodeVideoStreamProgressive(ffmpegPath, ffprobePath, input string, knownW, knownH int, hooks ProgressiveHooks) (*VideoDecodeResult, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	w, h := knownW, knownH
	if w <= 0 || h <= 0 {
		var err error
		w, h, err = ProbeVideoDimensions(ffprobePath, ffmpegPath, input)
		if err != nil {
			return nil, err
		}
	}

	isHTTP := strings.HasPrefix(strings.ToLower(input), "http://") || strings.HasPrefix(strings.ToLower(input), "https://")
	ramBudgetMB, lowPrio := GetResourceBudget()
	cfgWorkers := GetMaxWorkers()
	ffThreads := "0"
	if cfgWorkers > 0 && cfgWorkers < runtime.GOMAXPROCS(0) {
		ffThreads = strconv.Itoa(cfgWorkers)
	}
	args := []string{
		"-loglevel", "error",
		"-threads", ffThreads,
	}
	if isHTTP {
		args = append(args,
			"-reconnect", "1",
			"-reconnect_streamed", "1",
			"-reconnect_delay_max", "5",
		)
	}
	args = append(args,
		"-i", input,
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"pipe:1",
	)

	cmd := exec.Command(ffmpegPath, args...)
	sysutil.ApplySubprocessAttrs(cmd, lowPrio)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("erro iniciando FFmpeg para streaming progressivo: %w", err)
	}

	frame := NewYUVFrame(w, h)
	seen := map[uint32][]byte{}
	manifestSeen := map[uint32][]byte{}
	var (
		totalFrames     uint32
		containerID     uint64
		profileCode     string
		totalCorrected  int
		totalRaw        int
		manifestSpread  bool
		spreadStride    uint32
		manifestCount   uint32
		manifestEmitted bool
		manifestAborted bool
		parsedManifest  []byte
		rawFrameNum     uint32
		baseHint        DecodeHint
		phaseOffset     int
		cfSpec          CrossFrameSpec
		nextContigIdx   uint32
		contigInit      bool
		contigBuf       bytes.Buffer
		lastContigLen   int
		frameChunkSize  int
	)

	emitManifestIfReady := func() (abort bool) {
		if manifestEmitted || manifestCount == 0 {
			return false
		}
		var mb bytes.Buffer
		for i := uint32(0); i < manifestCount; i++ {
			ch, ok := manifestSeen[i]
			if !ok {
				return false
			}
			mb.Write(ch)
		}
		rawM := mb.Bytes()
		headerOff := 4
		if manifestSpread {
			headerOff = 8
		}
		if len(rawM) < headerOff {
			return false
		}
		mLen := int(binary.LittleEndian.Uint32(rawM[0:4]))
		if headerOff+mLen > len(rawM) {
			return false
		}
		encM := make([]byte, mLen)
		copy(encM, rawM[headerOff:headerOff+mLen])
		parsedManifest = encM
		if !manifestSpread && headerOff+mLen+CrossFrameTrailerSize <= len(rawM) {
			if sp, ok := ParseCrossFrameTrailer(rawM[headerOff+mLen:]); ok {
				cfSpec = sp
			}
		}
		manifestEmitted = true
		if hooks.OnManifest != nil {
			if !hooks.OnManifest(encM, int(totalFrames), profileCode, w, h, cfSpec) {
				manifestAborted = true
				return true
			}
		}
		return false
	}

	flushContiguous := func() {
		if !manifestEmitted || manifestSpread || manifestCount == 0 {
			return
		}
		if !contigInit {
			nextContigIdx = manifestCount
			contigInit = true
		}
		dataEnd := totalFrames
		if cfSpec.Enabled() {
			dataEnd = manifestCount + uint32(cfSpec.DataFrames)
		}
		for nextContigIdx < dataEnd {
			ch, ok := seen[nextContigIdx]
			if !ok {
				break
			}
			contigBuf.Write(ch)
			nextContigIdx++
		}
		if contigBuf.Len() > lastContigLen && hooks.OnContiguous != nil {
			lastContigLen = contigBuf.Len()
			hooks.OnContiguous(contigBuf.Bytes(), len(manifestSeen)+len(seen), int(totalFrames))
		}
		if hooks.OnSparseFrame != nil && frameChunkSize > 0 {
			hooks.OnSparseFrame(seen, manifestCount, frameChunkSize, len(manifestSeen)+len(seen), int(totalFrames))
		}
	}

	recordResult := func(res *DecodeFrameResult) (stopNow bool) {
		if totalFrames == 0 {
			totalFrames = res.Header.TotalFrames
			containerID = res.Header.ContainerID
			if p, ok := Profiles[res.Header.ProfileID]; ok {
				profileCode = p.Code
			}
		}
		idx := res.Header.FrameIndex
		if res.Header.Flags&FlagManifest != 0 {
			if res.Header.Flags&FlagSpread != 0 {
				manifestSpread = true
			}
			if _, exists := manifestSeen[idx]; !exists {
				buf := make([]byte, len(res.Payload))
				copy(buf, res.Payload)
				manifestSeen[idx] = buf
				totalCorrected += res.CorrectedBytes
				totalRaw += res.RawByteCount
				if idx == 0 && manifestSpread && len(buf) >= 8 {
					spreadStride = binary.LittleEndian.Uint32(buf[4:8])
				}
			}
		} else {
			if manifestCount == 0 {
				manifestCount = uint32(len(manifestSeen))
			}
			if !manifestEmitted {
				if emitManifestIfReady() {
					return true
				}
			}
			if _, exists := seen[idx]; !exists {
				buf := make([]byte, len(res.Payload))
				copy(buf, res.Payload)
				seen[idx] = buf
				if frameChunkSize == 0 && len(buf) > 0 {
					// Non-final data frames carry the exact uniform per-frame payload capacity!
					if idx+1 < totalFrames || totalFrames <= manifestCount+1 {
						frameChunkSize = len(buf)
					}
				} else if len(buf) > frameChunkSize {
					frameChunkSize = len(buf)
				}
				totalCorrected += res.CorrectedBytes
				totalRaw += res.RawByteCount
			}
			flushContiguous()
		}
		if hooks.OnProgress != nil && totalFrames > 0 {
			hooks.OnProgress(len(manifestSeen)+len(seen), int(totalFrames))
		}
		if totalFrames > 0 {
			if uint32(len(manifestSeen)+len(seen)) >= totalFrames {
				return true
			}
			if cfSpec.Enabled() && manifestCount > 0 && len(seen) >= cfSpec.DataFrames && CanReconstructCrossFrame(cfSpec, seen, manifestCount) {
				return true
			}
		}
		return false
	}

	// Phase 1: decode initial frames synchronously until HeaderHint + manifestCount are locked.
	readyForParallel := false
	for {
		err := readFullChunked(stdout, frame.Buf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("erro lendo frame do stream FFmpeg: %w", err)
		}
		curRawIdx := rawFrameNum
		rawFrameNum++
		curHint := baseHint
		if curHint.HeaderKind != HeaderKindUnknown {
			curHint.ExpectedPhase = ((int(curRawIdx) + phaseOffset) % 8 + 8) % 8
		}
		res, decErr := DecodeFrameWithHint(frame, curHint)
		if decErr != nil {
			if len(seen) == 0 && len(manifestSeen) == 0 {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return nil, decErr
			}
			if manifestSpread && manifestCount == 0 && len(manifestSeen) > 0 {
				manifestCount = uint32(len(manifestSeen))
				if emitManifestIfReady() {
					break
				}
				readyForParallel = true
				break
			}
			continue
		}
		if baseHint.HeaderKind == HeaderKindUnknown {
			baseHint = DecodeHint{
				HeaderKind: res.HeaderKind,
				ProfileID:  res.Header.ProfileID,
			}
			phaseOffset = int(res.Header.FrameIndex) - int(curRawIdx)
		}
		if recordResult(res) {
			break
		}
		if totalFrames > 2 && manifestCount > 0 {
			readyForParallel = true
			break
		}
	}

	// Phase 2: multi-core parallel frame decoding while keeping sequential FFmpeg pipe for progressive order!
	if readyForParallel && !manifestAborted && totalFrames > uint32(len(manifestSeen)+len(seen)) {
		numWorkers := cfgWorkers
		if numWorkers <= 0 {
			numWorkers = runtime.GOMAXPROCS(0)
		}
		maxWorkers := 12
		if w >= 3840 {
			maxWorkers = 8
		}
		if ramBudgetMB > 0 {
			frameMB := float64(w*h*3/2) / (1024 * 1024)
			ramCap := int(float64(ramBudgetMB)*0.35/frameMB) - 2
			if ramCap < 1 {
				ramCap = 1
			}
			if numWorkers > ramCap {
				numWorkers = ramCap
			}
		}
		if numWorkers > maxWorkers {
			numWorkers = maxWorkers
		}
		if numWorkers < 1 {
			numWorkers = 1
		}

		type decodeWork struct {
			frame  *YUVFrame
			rawIdx uint32
		}
		bufCount := numWorkers + 2
		freeBufs := make(chan *YUVFrame, bufCount)
		freeBufs <- frame
		for i := 1; i < bufCount; i++ {
			freeBufs <- NewYUVFrame(w, h)
		}

		workCh := make(chan decodeWork, bufCount)
		resCh := make(chan *DecodeFrameResult, numWorkers*2)
		stopCh := make(chan struct{})
		var stopOnce sync.Once
		stopAll := func() { stopOnce.Do(func() { close(stopCh) }) }

		wBaseHint := baseHint
		wPhaseOffset := phaseOffset
		wSpreadStride := spreadStride
		wManifestCount := manifestCount
		startRawIdx := rawFrameNum

		var readerErr error
		var readerErrMu sync.Mutex
		go func() {
			defer close(workCh)
			curRaw := startRawIdx
			for {
				var fr *YUVFrame
				select {
				case fr = <-freeBufs:
				case <-stopCh:
					return
				}
				if err := readFullChunked(stdout, fr.Buf); err != nil {
					freeBufs <- fr
					if err != io.EOF && err != io.ErrUnexpectedEOF {
						select {
						case <-stopCh:
						default:
							readerErrMu.Lock()
							readerErr = err
							readerErrMu.Unlock()
						}
					}
					return
				}
				idxNow := curRaw
				curRaw++
				if wSpreadStride >= 4 && wManifestCount > 0 && idxNow >= wManifestCount {
					rel := (idxNow - wManifestCount) % wSpreadStride
					if rel > 1 && rel < wSpreadStride-1 {
						freeBufs <- fr
						continue
					}
				}
				select {
				case workCh <- decodeWork{frame: fr, rawIdx: idxNow}:
				case <-stopCh:
					freeBufs <- fr
					return
				}
			}
		}()

		var wg sync.WaitGroup
		for wi := 0; wi < numWorkers; wi++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range workCh {
					hHint := wBaseHint
					hHint.ExpectedPhase = ((int(item.rawIdx) + wPhaseOffset) % 8 + 8) % 8
					res, err := DecodeFrameWithHint(item.frame, hHint)
					freeBufs <- item.frame
					if err != nil {
						continue
					}
					select {
					case resCh <- res:
					case <-stopCh:
						return
					}
				}
			}()
		}
		go func() {
			wg.Wait()
			close(resCh)
		}()

		for res := range resCh {
			if recordResult(res) {
				stopAll()
				_ = cmd.Process.Kill()
				break
			}
		}
		stopAll()
		for range resCh {
		}
		readerErrMu.Lock()
		rErr := readerErr
		readerErrMu.Unlock()
		if rErr != nil && !manifestAborted && uint32(len(manifestSeen)+len(seen)) < totalFrames {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("erro lendo frame do stream FFmpeg: %w", rErr)
		}
	}

	if manifestAborted || (totalFrames > 0 && uint32(len(manifestSeen)+len(seen)) >= totalFrames) ||
		(cfSpec.Enabled() && manifestCount > 0 && CanReconstructCrossFrame(cfSpec, seen, manifestCount)) {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()

	if len(manifestSeen) == 0 {
		return nil, fmt.Errorf("nenhum frame YTCloud válido encontrado no stream (%s)", strings.TrimSpace(stderr.String()))
	}
	if manifestCount == 0 {
		manifestCount = uint32(len(manifestSeen))
	}
	if !manifestEmitted {
		emitManifestIfReady()
	}
	if len(parsedManifest) == 0 {
		return nil, errors.New("manifesto incompleto ou truncado no stream")
	}
	if manifestAborted {
		return &VideoDecodeResult{
			ContainerID:         containerID,
			ProfileCode:         profileCode,
			Width:               w,
			Height:              h,
			TotalFrames:         int(totalFrames),
			DecodedFrames:       len(manifestSeen) + len(seen),
			TotalCorrectedBytes: totalCorrected,
			TotalRawBytes:       totalRaw,
			EncryptedManifest:   parsedManifest,
		}, nil
	}

	// Phase 3 (Rescue): for local stealth-film files with missing stripes not yet covered by Cross-Frame RS
	if !isHTTP && !manifestSpread && totalFrames > 0 && baseHint.ProfileID == ProfileStealthFilm &&
		uint32(len(manifestSeen)+len(seen)) < totalFrames &&
		!(cfSpec.Enabled() && manifestCount > 0 && CanReconstructCrossFrame(cfSpec, seen, manifestCount)) {
		missing := make(map[uint32]bool)
		mCnt := uint32(len(manifestSeen))
		for i := uint32(0); i < mCnt; i++ {
			if _, ok := manifestSeen[i]; !ok {
				missing[i] = true
			}
		}
		if cfSpec.Enabled() {
			for s := 0; s < cfSpec.Stripes; s++ {
				have := 0
				var stripeMissing []uint32
				for p := 0; p < cfSpec.K; p++ {
					dataIdx := p*cfSpec.Stripes + s
					if dataIdx >= cfSpec.DataFrames {
						have++
						continue
					}
					fi := mCnt + uint32(dataIdx)
					if _, ok := seen[fi]; ok {
						have++
					} else {
						stripeMissing = append(stripeMissing, fi)
					}
				}
				for q := 0; q < cfSpec.M; q++ {
					parIdx := cfSpec.DataFrames + q*cfSpec.Stripes + s
					fi := mCnt + uint32(parIdx)
					if _, ok := seen[fi]; ok {
						have++
					} else {
						stripeMissing = append(stripeMissing, fi)
					}
				}
				if have < cfSpec.K {
					for _, fi := range stripeMissing {
						missing[fi] = true
					}
				}
			}
		} else {
			for i := mCnt; i < totalFrames; i++ {
				if _, ok := seen[i]; !ok {
					missing[i] = true
				}
			}
		}
		if len(missing) > 0 {
			rescueStealthFilmFrames(ffmpegPath, input, w, h, phaseOffset, missing, func(res *DecodeFrameResult) bool {
				return recordResult(res)
			})
		}
	}

	var encPayload []byte
	crossFrameRecovered := 0
	expectedData := int(totalFrames) - len(manifestSeen)
	if expectedData > 0 {
		if cfSpec.Enabled() {
			startIdx := uint32(len(manifestSeen))
			recPayload, recFrames, recErr := ReconstructCrossFramePayload(cfSpec, seen, startIdx)
			if recErr != nil {
				return nil, fmt.Errorf("recuperação Cross-Frame RS falhou: %w", recErr)
			}
			encPayload = recPayload
			crossFrameRecovered = recFrames
		} else if manifestSpread {
			var payloadBuf bytes.Buffer
			keys := make([]int, 0, len(seen))
			for k := range seen {
				keys = append(keys, int(k))
			}
			sort.Ints(keys)
			if len(keys) < expectedData {
				return nil, fmt.Errorf("frames de dados espalhados incompletos (%d/%d)", len(keys), expectedData)
			}
			for i := 0; i < expectedData; i++ {
				payloadBuf.Write(seen[uint32(keys[i])])
			}
			encPayload = payloadBuf.Bytes()
		} else {
			var payloadBuf bytes.Buffer
			startIdx := uint32(len(manifestSeen))
			for i := startIdx; i < totalFrames; i++ {
				chunk, ok := seen[i]
				if !ok {
					return nil, fmt.Errorf("frame de dados %d/%d ausente ou corrompido", i, totalFrames)
				}
				payloadBuf.Write(chunk)
			}
			encPayload = payloadBuf.Bytes()
		}
	}

	return &VideoDecodeResult{
		ContainerID:         containerID,
		ProfileCode:         profileCode,
		Width:               w,
		Height:              h,
		TotalFrames:         int(totalFrames),
		DecodedFrames:       len(manifestSeen) + len(seen),
		TotalCorrectedBytes: totalCorrected,
		TotalRawBytes:       totalRaw,
		CrossFrameRecovered: crossFrameRecovered,
		EncryptedManifest:   parsedManifest,
		EncryptedPayload:    encPayload,
	}, nil
}

func decodeVideoAtSize(ffmpegPath, videoPath string, w, h int, videoDurSec float64, rescale bool, manifestOnly bool, onProgress func(frame, total int)) (*VideoDecodeResult, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}

	ramBudgetMB, lowPrio := GetResourceBudget()
	cfgWorkers := GetMaxWorkers()
	ffThreads := "0"
	if cfgWorkers > 0 && cfgWorkers < runtime.GOMAXPROCS(0) {
		ffThreads = strconv.Itoa(cfgWorkers)
	}
	args := []string{
		"-loglevel", "error",
		"-threads", ffThreads,
		"-i", videoPath,
	}
	if rescale {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d:flags=bilinear", w, h))
	}
	args = append(args,
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"pipe:1",
	)
	cmd := exec.Command(ffmpegPath, args...)
	sysutil.ApplySubprocessAttrs(cmd, lowPrio)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("erro iniciando FFmpeg para decodificação: %w", err)
	}

	frame := NewYUVFrame(w, h)
	seen := map[uint32][]byte{}
	manifestSeen := map[uint32][]byte{}
	var (
		totalFrames       uint32
		containerID       uint64
		profileCode       string
		totalCorrected    int
		totalRaw          int
		manifestDone      bool
		manifestSpread    bool
		spreadStride      uint32
		manifestCount     uint32
		rawFrameNum       uint32
		baseHint          DecodeHint
		phaseOffset       int
		cfSpec            CrossFrameSpec
		cfSpecParsed      bool
		partitionedDecode bool
	)

	tryParseLiveCFSpec := func() {
		if cfSpecParsed || manifestSpread || manifestCount == 0 {
			return
		}
		var mb bytes.Buffer
		for i := uint32(0); i < manifestCount; i++ {
			ch, ok := manifestSeen[i]
			if !ok {
				return
			}
			mb.Write(ch)
		}
		rawM := mb.Bytes()
		if len(rawM) < 4 {
			return
		}
		mLen := int(binary.LittleEndian.Uint32(rawM[0:4]))
		if 4+mLen+CrossFrameTrailerSize <= len(rawM) {
			if sp, ok := ParseCrossFrameTrailer(rawM[4+mLen:]); ok {
				cfSpec = sp
			}
		}
		cfSpecParsed = true
	}

	recordResult := func(res *DecodeFrameResult) (stopNow bool) {
		if totalFrames == 0 {
			totalFrames = res.Header.TotalFrames
			containerID = res.Header.ContainerID
			if p, ok := Profiles[res.Header.ProfileID]; ok {
				profileCode = p.Code
			}
		}
		idx := res.Header.FrameIndex
		if res.Header.Flags&FlagManifest != 0 {
			if res.Header.Flags&FlagSpread != 0 {
				manifestSpread = true
			}
			if _, exists := manifestSeen[idx]; !exists {
				buf := make([]byte, len(res.Payload))
				copy(buf, res.Payload)
				manifestSeen[idx] = buf
				totalCorrected += res.CorrectedBytes
				totalRaw += res.RawByteCount
				if idx == 0 && manifestSpread && len(buf) >= 8 {
					spreadStride = binary.LittleEndian.Uint32(buf[4:8])
				}
			}
		} else {
			if manifestCount == 0 {
				manifestCount = uint32(len(manifestSeen))
				tryParseLiveCFSpec()
			}
			if manifestOnly {
				manifestDone = true
				return true
			}
			if _, exists := seen[idx]; !exists {
				buf := make([]byte, len(res.Payload))
				copy(buf, res.Payload)
				seen[idx] = buf
				totalCorrected += res.CorrectedBytes
				totalRaw += res.RawByteCount
			}
		}
		if onProgress != nil && totalFrames > 0 {
			onProgress(len(manifestSeen)+len(seen), int(totalFrames))
		}
		if !manifestOnly && totalFrames > 0 {
			if uint32(len(manifestSeen)+len(seen)) >= totalFrames {
				return true
			}
			// Early-stop for Cross-Frame RS: once every stripe has >= K frames (e.g. all data intact!), stop reading!
			if cfSpec.Enabled() && manifestCount > 0 && len(seen) >= cfSpec.DataFrames && CanReconstructCrossFrame(cfSpec, seen, manifestCount) {
				return true
			}
		}
		return false
	}

	// Phase 1: decode initial frames synchronously until we lock onto HeaderKind/ProfileID (and manifestCount in spread mode).
	readyForParallel := false
	for {
		err := readFullChunked(stdout, frame.Buf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("erro lendo frame do FFmpeg: %w", err)
		}

		curRawIdx := rawFrameNum
		rawFrameNum++

		curHint := baseHint
		if curHint.HeaderKind != HeaderKindUnknown {
			curHint.ExpectedPhase = ((int(curRawIdx) + phaseOffset) % 8 + 8) % 8
		}
		res, decErr := DecodeFrameWithHint(frame, curHint)
		if decErr != nil {
			if len(seen) == 0 && len(manifestSeen) == 0 {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return nil, decErr
			}
			if manifestOnly && len(manifestSeen) > 0 && manifestSpread {
				manifestDone = true
				break
			}
			if !manifestOnly && manifestSpread && manifestCount == 0 && len(manifestSeen) > 0 {
				manifestCount = uint32(len(manifestSeen))
				readyForParallel = true
				break
			}
			continue
		}

		if baseHint.HeaderKind == HeaderKindUnknown {
			baseHint = DecodeHint{
				HeaderKind: res.HeaderKind,
				ProfileID:  res.Header.ProfileID,
			}
			phaseOffset = int(res.Header.FrameIndex) - int(curRawIdx)
		}

		if recordResult(res) {
			break
		}

		// Launch multi-core parallel worker pool as soon as we know the header format AND manifestCount!
		if !manifestOnly && totalFrames > 2 && manifestCount > 0 {
			readyForParallel = true
			break
		}
	}

	// Phase 2A: Automatic 4x Partitioned Parallel Decode (0, 0.25, 0.50, 0.75) on large videos!
	// Empirically ~2.0x faster than single-pipe FFmpeg decode on 1080p/2K/4K videos >= 120 frames,
	// 100% backwards-compatible with ALL existing videos (uses FrameIndex deduplication!).
	effWorkers := cfgWorkers
	if effWorkers <= 0 {
		effWorkers = runtime.GOMAXPROCS(0)
	}
	usePartitioned4x := readyForParallel && !manifestSpread && totalFrames >= 120 && videoDurSec >= 6.0 && effWorkers >= 4 && w < 7680
	if usePartitioned4x && ramBudgetMB > 0 {
		// Require enough RAM budget for 4 concurrent decoders
		frameMB := float64(w*h*3/2) / (1024 * 1024)
		if float64(ramBudgetMB)*0.35 < frameMB*8 {
			usePartitioned4x = false
		}
	}

	if usePartitioned4x && totalFrames > uint32(len(manifestSeen)+len(seen)) {
		// Close the Phase 1 probe FFmpeg process and launch 4 parallel FFmpeg readers across [0..videoDurSec]!
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		partitionedDecode = true

		var recMu sync.Mutex
		decodeVideoPartitions4x(
			ffmpegPath, videoPath, w, h, rescale, videoDurSec,
			int(totalFrames), baseHint, effWorkers, lowPrio,
			func(res *DecodeFrameResult) bool {
				recMu.Lock()
				stop := recordResult(res)
				recMu.Unlock()
				return stop
			},
		)
		// Ensure manifestDone/cmd.Wait won't double-wait
		readyForParallel = false
		cmd = nil
	}

	// Phase 2B: Multi-core bounded parallel frame decoding (single FFmpeg pipe for short/spread videos)
	if readyForParallel && totalFrames > uint32(len(manifestSeen)+len(seen)) {
		numWorkers := cfgWorkers
		if numWorkers <= 0 {
			numWorkers = runtime.GOMAXPROCS(0)
		}
		maxWorkers := 12
		if w >= 3840 {
			maxWorkers = 8 // cap 4K frame-buffer RAM around ~120 MB
		}
		if ramBudgetMB > 0 {
			frameMB := float64(w*h*3/2) / (1024 * 1024)
			ramCap := int(float64(ramBudgetMB)*0.35/frameMB) - 2
			if ramCap < 1 {
				ramCap = 1
			}
			if numWorkers > ramCap {
				numWorkers = ramCap
			}
		}
		if numWorkers > maxWorkers {
			numWorkers = maxWorkers
		}
		if numWorkers < 1 {
			numWorkers = 1
		}

		type decodeWork struct {
			frame  *YUVFrame
			rawIdx uint32
		}
		bufCount := numWorkers + 2
		freeBufs := make(chan *YUVFrame, bufCount)
		freeBufs <- frame // reuse the frame we already allocated!
		for i := 1; i < bufCount; i++ {
			freeBufs <- NewYUVFrame(w, h)
		}

		workCh := make(chan decodeWork, bufCount)
		resCh := make(chan *DecodeFrameResult, numWorkers*2)
		stopCh := make(chan struct{})
		var stopOnce sync.Once
		stopAll := func() {
			stopOnce.Do(func() { close(stopCh) })
		}

		// Capture immutable hints for worker goroutines
		wBaseHint := baseHint
		wPhaseOffset := phaseOffset
		wSpreadStride := spreadStride
		wManifestCount := manifestCount
		startRawIdx := rawFrameNum

		// Reader goroutine: streams YUV420p frames from FFmpeg into recycled buffers & dispatches to workers
		var readerErr error
		var readerErrMu sync.Mutex
		go func() {
			defer close(workCh)
			curRaw := startRawIdx
			for {
				var fr *YUVFrame
				select {
				case fr = <-freeBufs:
				case <-stopCh:
					return
				}
				if err := readFullChunked(stdout, fr.Buf); err != nil {
					freeBufs <- fr
					if err != io.EOF && err != io.ErrUnexpectedEOF {
						select {
						case <-stopCh:
						default:
							readerErrMu.Lock()
							readerErr = err
							readerErrMu.Unlock()
						}
					}
					return
				}
				idxNow := curRaw
				curRaw++

				// Fast-skip intermediate clean frames in Spread mode (keeps ±1 frame tolerance)
				if wSpreadStride >= 4 && wManifestCount > 0 && idxNow >= wManifestCount {
					rel := (idxNow - wManifestCount) % wSpreadStride
					if rel > 1 && rel < wSpreadStride-1 {
						freeBufs <- fr
						continue
					}
				}

				select {
				case workCh <- decodeWork{frame: fr, rawIdx: idxNow}:
				case <-stopCh:
					freeBufs <- fr
					return
				}
			}
		}()

		// Worker goroutines: decode frames concurrently in parallel using HeaderHint fast-path!
		var wg sync.WaitGroup
		for wi := 0; wi < numWorkers; wi++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range workCh {
					h := wBaseHint
					h.ExpectedPhase = ((int(item.rawIdx) + wPhaseOffset) % 8 + 8) % 8
					res, err := DecodeFrameWithHint(item.frame, h)
					freeBufs <- item.frame // guaranteed non-blocking (cap == total buffers)
					if err != nil {
						continue
					}
					select {
					case resCh <- res:
					case <-stopCh:
						return
					}
				}
			}()
		}
		go func() {
			wg.Wait()
			close(resCh)
		}()

		// Collector loop on main goroutine
		for res := range resCh {
			if recordResult(res) {
				stopAll()
				_ = cmd.Process.Kill()
				break
			}
		}
		stopAll()
		// Drain resCh briefly after stopAll so worker/closer goroutines exit cleanly
		for range resCh {
		}
		readerErrMu.Lock()
		rErr := readerErr
		readerErrMu.Unlock()
		if rErr != nil && uint32(len(manifestSeen)+len(seen)) < totalFrames {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("erro lendo frame do FFmpeg: %w", rErr)
		}
	}

	if cmd != nil {
		if manifestDone || (totalFrames > 0 && uint32(len(manifestSeen)+len(seen)) >= totalFrames) ||
			(cfSpec.Enabled() && manifestCount > 0 && CanReconstructCrossFrame(cfSpec, seen, manifestCount)) {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}

	// Safety gap-fill: if 4x partitioned seek missed a boundary frame on an unusual VFR video and
	// Cross-Frame RS cannot already reconstruct it, do one sequential sweep to collect missing frames!
	if partitionedDecode && !manifestOnly && totalFrames > 0 && uint32(len(manifestSeen)+len(seen)) < totalFrames &&
		!(cfSpec.Enabled() && manifestCount > 0 && CanReconstructCrossFrame(cfSpec, seen, manifestCount)) {
		seqFillMissingFrames(ffmpegPath, videoPath, w, h, rescale, baseHint, phaseOffset, lowPrio, func(res *DecodeFrameResult) bool {
			return recordResult(res)
		})
	}

	// Phase 3 (Rescue): if any ProfileStealthFilm frames failed due to YouTube VP9 inter-frame
	// temporal attenuation on static/slow backgrounds (and Cross-Frame RS hasn't already covered them!),
	// run a lightweight sliding-window temporal host-leakage cancellation pass ONLY on frames belonging
	// to stripes that still need recovery!
	if !manifestOnly && !manifestSpread && totalFrames > 0 && baseHint.ProfileID == ProfileStealthFilm &&
		uint32(len(manifestSeen)+len(seen)) < totalFrames &&
		!(cfSpec.Enabled() && manifestCount > 0 && CanReconstructCrossFrame(cfSpec, seen, manifestCount)) {
		missing := make(map[uint32]bool)
		mCnt := uint32(len(manifestSeen))
		for i := uint32(0); i < mCnt; i++ {
			if _, ok := manifestSeen[i]; !ok {
				missing[i] = true
			}
		}
		if cfSpec.Enabled() {
			// Only target stripes that currently have < K frames (skips already-recoverable stripes -> 5x-10x faster!)
			for s := 0; s < cfSpec.Stripes; s++ {
				have := 0
				var stripeMissing []uint32
				for p := 0; p < cfSpec.K; p++ {
					dataIdx := p*cfSpec.Stripes + s
					if dataIdx >= cfSpec.DataFrames {
						have++
						continue
					}
					fi := mCnt + uint32(dataIdx)
					if _, ok := seen[fi]; ok {
						have++
					} else {
						stripeMissing = append(stripeMissing, fi)
					}
				}
				for q := 0; q < cfSpec.M; q++ {
					parIdx := cfSpec.DataFrames + q*cfSpec.Stripes + s
					fi := mCnt + uint32(parIdx)
					if _, ok := seen[fi]; ok {
						have++
					} else {
						stripeMissing = append(stripeMissing, fi)
					}
				}
				if have < cfSpec.K {
					for _, fi := range stripeMissing {
						missing[fi] = true
					}
				}
			}
		} else {
			for i := mCnt; i < totalFrames; i++ {
				if _, ok := seen[i]; !ok {
					missing[i] = true
				}
			}
		}
		if len(missing) > 0 {
			rescueStealthFilmFrames(ffmpegPath, videoPath, w, h, phaseOffset, missing, func(res *DecodeFrameResult) bool {
				return recordResult(res)
			})
		}
	}

	if len(manifestSeen) == 0 {
		return nil, fmt.Errorf("nenhum frame YTCloud válido encontrado no vídeo (%s)", strings.TrimSpace(stderr.String()))
	}

	// Reassemble manifest frames in order 0..M-1
	var manifestRaw bytes.Buffer
	for i := uint32(0); i < uint32(len(manifestSeen)); i++ {
		chunk, ok := manifestSeen[i]
		if !ok {
			return nil, fmt.Errorf("frame de manifesto %d ausente", i)
		}
		manifestRaw.Write(chunk)
	}
	mBytes := manifestRaw.Bytes()
	headerOff := 4
	if manifestSpread {
		headerOff = 8 // [4B mLen][4B spreadStride]
	}
	if len(mBytes) < headerOff {
		return nil, errors.New("manifesto truncado")
	}
	mLen := int(binary.LittleEndian.Uint32(mBytes[0:4]))
	if headerOff+mLen > len(mBytes) {
		return nil, fmt.Errorf("manifesto incompleto (%d/%d bytes)", len(mBytes)-headerOff, mLen)
	}
	encManifest := make([]byte, mLen)
	copy(encManifest, mBytes[headerOff:headerOff+mLen])
	if !cfSpec.Enabled() && !manifestSpread && headerOff+mLen+CrossFrameTrailerSize <= len(mBytes) {
		if sp, ok := ParseCrossFrameTrailer(mBytes[headerOff+mLen:]); ok {
			cfSpec = sp
		}
	}

	var encPayload []byte
	crossFrameRecovered := 0
	if !manifestOnly {
		expectedData := int(totalFrames) - len(manifestSeen)
		if expectedData > 0 {
			if cfSpec.Enabled() {
				startIdx := uint32(len(manifestSeen))
				recPayload, recFrames, recErr := ReconstructCrossFramePayload(cfSpec, seen, startIdx)
				if recErr != nil {
					return nil, fmt.Errorf("recuperação Cross-Frame RS falhou: %w", recErr)
				}
				encPayload = recPayload
				crossFrameRecovered = recFrames
			} else if manifestSpread {
				var payloadBuf bytes.Buffer
				// Sort spread frame indices ascending (strictly preserves exact chunk order!)
				keys := make([]int, 0, len(seen))
				for k := range seen {
					keys = append(keys, int(k))
				}
				sort.Ints(keys)
				if len(keys) < expectedData {
					return nil, fmt.Errorf("frames de dados espalhados incompletos (%d/%d)", len(keys), expectedData)
				}
				for i := 0; i < expectedData; i++ {
					payloadBuf.Write(seen[uint32(keys[i])])
				}
				encPayload = payloadBuf.Bytes()
			} else {
				var payloadBuf bytes.Buffer
				startIdx := uint32(len(manifestSeen))
				for i := startIdx; i < totalFrames; i++ {
					chunk, ok := seen[i]
					if !ok {
						return nil, fmt.Errorf("frame de dados %d/%d ausente ou corrompido", i, totalFrames)
					}
					payloadBuf.Write(chunk)
				}
				encPayload = payloadBuf.Bytes()
			}
		}
	}

	return &VideoDecodeResult{
		ContainerID:         containerID,
		ProfileCode:         profileCode,
		Width:               w,
		Height:              h,
		TotalFrames:         int(totalFrames),
		DecodedFrames:       len(manifestSeen) + len(seen),
		TotalCorrectedBytes: totalCorrected,
		TotalRawBytes:       totalRaw,
		CrossFrameRecovered: crossFrameRecovered,
		PartitionedDecode:   partitionedDecode,
		EncryptedManifest:   encManifest,
		EncryptedPayload:    encPayload,
	}, nil
}

// decodeVideoPartitions4x runs 4 concurrent FFmpeg readers across [0.00, 0.25, 0.50, 0.75] of videoDurSec
// with keyframe-safe boundary overlap and stops all 4 immediately as soon as onFrame signals completion!
func decodeVideoPartitions4x(
	ffmpegPath, videoPath string, w, h int, rescale bool, videoDurSec float64,
	totalFrames int, baseHint DecodeHint, effWorkers int, lowPrio bool,
	onFrame func(*DecodeFrameResult) bool,
) {
	const numParts = 4
	threadsPerPart := effWorkers / numParts
	if threadsPerPart < 1 {
		threadsPerPart = 1
	}
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stopAll := func() { stopOnce.Do(func() { close(stopCh) }) }

	partDur := videoDurSec / float64(numParts)
	var wg sync.WaitGroup
	for pi := 0; pi < numParts; pi++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			startSec := float64(idx)*partDur - 0.35
			if idx == 0 || startSec < 0 {
				startSec = 0
			}
			durSec := partDur + 0.95
			args := []string{"-loglevel", "error", "-threads", strconv.Itoa(threadsPerPart)}
			if startSec > 0 {
				args = append(args, "-ss", fmt.Sprintf("%.3f", startSec))
			}
			args = append(args, "-i", videoPath)
			if idx < numParts-1 {
				args = append(args, "-t", fmt.Sprintf("%.3f", durSec))
			}
			if rescale {
				args = append(args, "-vf", fmt.Sprintf("scale=%d:%d:flags=bilinear", w, h))
			}
			args = append(args, "-f", "rawvideo", "-pix_fmt", "yuv420p", "pipe:1")

			cmd := exec.Command(ffmpegPath, args...)
			sysutil.ApplySubprocessAttrs(cmd, lowPrio)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				return
			}
			if err := cmd.Start(); err != nil {
				return
			}
			defer func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}()

			// Watch stopCh so all 4 FFmpeg instances terminate immediately on early completion
			doneLocal := make(chan struct{})
			defer close(doneLocal)
			go func() {
				select {
				case <-stopCh:
					_ = cmd.Process.Kill()
				case <-doneLocal:
				}
			}()

			fr := NewYUVFrame(w, h)
			partHint := baseHint
			phaseLocked := false
			partPhaseOff := 0
			localRaw := 0
			for {
				select {
				case <-stopCh:
					return
				default:
				}
				if err := readFullChunked(stdout, fr.Buf); err != nil {
					return
				}
				hNow := partHint
				if phaseLocked {
					hNow.ExpectedPhase = ((localRaw + partPhaseOff) % 8 + 8) % 8
				}
				res, decErr := DecodeFrameWithHint(fr, hNow)
				localRaw++
				if decErr != nil {
					continue
				}
				if !phaseLocked {
					partPhaseOff = int(res.Header.FrameIndex) - (localRaw - 1)
					phaseLocked = true
				}
				if onFrame(res) {
					stopAll()
					return
				}
			}
		}(pi)
	}
	wg.Wait()
}

// seqFillMissingFrames performs a clean sequential sweep if 4x partitioned seek missed a boundary frame.
func seqFillMissingFrames(ffmpegPath, videoPath string, w, h int, rescale bool, baseHint DecodeHint, phaseOffset int, lowPrio bool, onFrame func(*DecodeFrameResult) bool) {
	args := []string{"-loglevel", "error", "-threads", "0", "-i", videoPath}
	if rescale {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d:flags=bilinear", w, h))
	}
	args = append(args, "-f", "rawvideo", "-pix_fmt", "yuv420p", "pipe:1")
	cmd := exec.Command(ffmpegPath, args...)
	sysutil.ApplySubprocessAttrs(cmd, lowPrio)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	fr := NewYUVFrame(w, h)
	rawIdx := 0
	for {
		if err := readFullChunked(stdout, fr.Buf); err != nil {
			return
		}
		hNow := baseHint
		if hNow.HeaderKind != HeaderKindUnknown {
			hNow.ExpectedPhase = ((rawIdx + phaseOffset) % 8 + 8) % 8
		}
		res, decErr := DecodeFrameWithHint(fr, hNow)
		rawIdx++
		if decErr != nil {
			continue
		}
		if onFrame(res) {
			return
		}
	}
}

// rescueStealthFilmFrames streams the video once with a sliding window of Y planes (radius=7 -> 14 neighbors,
// all guaranteed to have different phase != i%8!) and rescues ONLY missing ProfileStealthFilm frames in parallel
// via temporal host-leakage cancellation (Y_t - alpha*BG) + scene-cut-aware fallbacks (prev5 / next5 / tight2).
func rescueStealthFilmFrames(ffmpegPath, videoPath string, w, h, phaseOffset int, missing map[uint32]bool, onRecovered func(*DecodeFrameResult) bool) {
	cmd := exec.Command(ffmpegPath,
		"-v", "error",
		"-threads", "0",
		"-i", videoPath,
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"-",
	)
	sysutil.ApplySubprocessAttrs(cmd, true)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	const radius = 7
	type winEntry struct {
		rawIdx int
		yPlane []byte
	}
	type rescueJob struct {
		target  []byte
		phase   int
		allNB   [][]byte
		prevNB  [][]byte
		nextNB  [][]byte
		prev1NB [][]byte
		tightNB [][]byte
	}

	ySize := w * h
	tmpFrame := NewYUVFrame(w, h)
	var window []winEntry

	// Quick sampled MAD (~512 pixels) to reject neighbors from a different scene across a scene cut
	quickMAD := func(a, b []byte) float64 {
		step := len(a) / 512
		if step < 1 {
			step = 1
		}
		var sum, cnt int
		for i := 0; i < len(a); i += step {
			d := int(a[i]) - int(b[i])
			if d < 0 {
				d = -d
			}
			sum += d
			cnt++
		}
		if cnt == 0 {
			return 0
		}
		return float64(sum) / float64(cnt)
	}

	workers := GetMaxWorkers()
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}
	jobCh := make(chan rescueJob, workers*2)
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stopAll := func() { stopOnce.Do(func() { close(stopCh) }) }
	var recMu sync.Mutex
	var wg sync.WaitGroup
	for wi := 0; wi < workers; wi++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				select {
				case <-stopCh:
					continue
				default:
				}
				srcView := &YUVFrame{Width: w, Height: h, Buf: job.target}
				candidates := [][][]byte{job.allNB, job.prevNB, job.nextNB, job.prev1NB, job.tightNB}
				res, decErr := DecodeStealthFilmWithNeighborSets(srcView, candidates, job.phase)
				if decErr == nil && res != nil {
					recMu.Lock()
					doneNow := onRecovered(res)
					recMu.Unlock()
					if doneNow {
						stopAll()
					}
				}
			}
		}()
	}

	pendingMissing := make(map[uint32]bool, len(missing))
	for k := range missing {
		pendingMissing[k] = true
	}

	dispatchEntry := func(centerPos int) {
		if len(pendingMissing) == 0 {
			return
		}
		target := window[centerPos]
		estIdx := target.rawIdx + phaseOffset
		if estIdx < 0 || !pendingMissing[uint32(estIdx)] {
			return
		}
		delete(pendingMissing, uint32(estIdx))

		var allNB, sameSceneNB, prevNB, nextNB, prev1NB, tightNB [][]byte
		for i, we := range window {
			if i == centerPos {
				continue
			}
			// Never include a neighbor with identical 8-frame phase (its grain would correlate!)
			if ((we.rawIdx - target.rawIdx) % 8) == 0 {
				continue
			}
			allNB = append(allNB, we.yPlane)
			if quickMAD(target.yPlane, we.yPlane) < 26.0 {
				sameSceneNB = append(sameSceneNB, we.yPlane)
			}
			dist := i - centerPos
			if dist < 0 && dist >= -7 {
				prevNB = append(prevNB, we.yPlane)
			} else if dist > 0 && dist <= 7 {
				nextNB = append(nextNB, we.yPlane)
			}
			if dist == -1 {
				prev1NB = append(prev1NB, we.yPlane)
			}
			if dist == -1 || dist == 1 {
				tightNB = append(tightNB, we.yPlane)
			}
		}
		primary := sameSceneNB
		if len(primary) < 2 {
			primary = allNB
		}
		if len(primary) == 0 {
			return
		}
		jobCh <- rescueJob{
			target:  target.yPlane,
			phase:   ((estIdx % 8) + 8) % 8,
			allNB:   primary,
			prevNB:  prevNB,
			nextNB:  nextNB,
			prev1NB: prev1NB,
			tightNB: tightNB,
		}
	}

	rawIdx := 0
	for len(pendingMissing) > 0 {
		select {
		case <-stopCh:
			pendingMissing = nil
			continue
		default:
		}
		if err := readFullChunked(stdout, tmpFrame.Buf); err != nil {
			break
		}
		yCopy := make([]byte, ySize)
		copy(yCopy, tmpFrame.Buf[:ySize])
		window = append(window, winEntry{rawIdx: rawIdx, yPlane: yCopy})
		rawIdx++

		if len(window) > radius {
			centerPos := len(window) - 1 - radius
			dispatchEntry(centerPos)
		}
		if len(window) > 2*radius+1 {
			window = window[1:]
		}
	}
	startTail := len(window) - radius
	if startTail < 0 {
		startTail = 0
	}
	for pos := startTail; pos < len(window) && len(pendingMissing) > 0; pos++ {
		dispatchEntry(pos)
	}
	close(jobCh)
	wg.Wait()
}

// CalibrationResult reports round-trip survival metrics for a modulation profile under lossy compression.
type CalibrationResult struct {
	ProfileCode     string  `json:"profile_code"`
	ProfileName     string  `json:"profile_name"`
	Resolution      string  `json:"resolution"`
	CRF             int     `json:"crf"`
	PayloadPerFrame int     `json:"payload_per_frame"`
	Passed          bool    `json:"passed"`
	CorrectedBytes  int     `json:"corrected_bytes"`
	RawBytes        int     `json:"raw_bytes"`
	ByteErrorRate   float64 `json:"byte_error_rate_pct"`
	Error           string  `json:"error,omitempty"`
}

// RunLocalCalibration simulates lossy YouTube-like recompression (H.264/CRF) locally for a profile.
func RunLocalCalibration(ffmpegPath, ffprobePath, tempDir, resCode, profCode string, simCRF int) CalibrationResult {
	prof, err := ProfileByCode(profCode)
	if err != nil {
		return CalibrationResult{ProfileCode: profCode, Resolution: resCode, Error: err.Error()}
	}
	res, err := ResolutionByCode(resCode)
	if err != nil {
		return CalibrationResult{ProfileCode: profCode, ProfileName: prof.Name, Resolution: resCode, Error: err.Error()}
	}
	if simCRF <= 0 {
		simCRF = 26 // simulates aggressive YouTube lossy transcoding
	}
	capBytes := prof.PayloadCapacity(res.Width, res.Height)
	testManifest := []byte(`{"test":"ytcloud-calibration","v":1}`)
	testPayload := make([]byte, capBytes)
	// Deterministic high-entropy pseudo-random payload
	var x uint64 = 0x9E3779B97F4A7C15
	for i := range testPayload {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		testPayload[i] = byte(x)
	}

	outPath := filepath.Join(tempDir, fmt.Sprintf("cal_%s_%s.mp4", resCode, profCode))
	defer os.Remove(outPath)

	_, err = EncodeStreamToVideo(VideoEncodeOptions{
		FFmpegPath:     ffmpegPath,
		OutputPath:     outPath,
		ResolutionCode: resCode,
		ProfileCode:    profCode,
		VideoCodec:     "h264",
		FPS:            10,
		CRF:            simCRF,
		ContainerID:    0xCAFEBABE,
	}, testManifest, testPayload)
	if err != nil {
		return CalibrationResult{
			ProfileCode: prof.Code, ProfileName: prof.Name, Resolution: resCode, CRF: simCRF,
			PayloadPerFrame: capBytes, Error: err.Error(),
		}
	}

	dec, err := DecodeVideoStream(ffmpegPath, ffprobePath, outPath, false, nil)
	if err != nil {
		return CalibrationResult{
			ProfileCode: prof.Code, ProfileName: prof.Name, Resolution: resCode, CRF: simCRF,
			PayloadPerFrame: capBytes, Error: err.Error(),
		}
	}

	passed := bytes.Equal(dec.EncryptedManifest, testManifest) && bytes.Equal(dec.EncryptedPayload, testPayload)
	ber := 0.0
	if dec.TotalRawBytes > 0 {
		ber = (float64(dec.TotalCorrectedBytes) / float64(dec.TotalRawBytes)) * 100.0
	}
	return CalibrationResult{
		ProfileCode:     prof.Code,
		ProfileName:     prof.Name,
		Resolution:      resCode,
		CRF:             simCRF,
		PayloadPerFrame: capBytes,
		Passed:          passed,
		CorrectedBytes:  dec.TotalCorrectedBytes,
		RawBytes:        dec.TotalRawBytes,
		ByteErrorRate:   ber,
	}
}

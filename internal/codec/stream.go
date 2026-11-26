package codec

import (
	"bytes"
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
	OnProgress     func(frame, total int)
}

// VideoEncodeStats summarizes the generated video container.
type VideoEncodeStats struct {
	OutputPath      string `json:"output_path"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	FPS             int    `json:"fps"`
	ProfileCode     string `json:"profile_code"`
	VideoCodec      string `json:"video_codec"`
	ManifestFrames  int    `json:"manifest_frames"`
	DataFrames      int    `json:"data_frames"`
	TotalFrames     int    `json:"total_frames"`
	VideoSizeBytes  int64  `json:"video_size_bytes"`
	PayloadPerFrame int    `json:"payload_per_frame"`
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
	if spreadStride > 1 {
		// [4B mLen][4B spreadStride][encryptedManifest] signaled via FlagSpread on manifest frames
		manifestBlob = make([]byte, 8+len(encryptedManifest))
		binary.LittleEndian.PutUint32(manifestBlob[0:4], uint32(len(encryptedManifest)))
		binary.LittleEndian.PutUint32(manifestBlob[4:8], uint32(spreadStride))
		copy(manifestBlob[8:], encryptedManifest)
	} else {
		// Standard [4B mLen][encryptedManifest] (100% backwards-compatible)
		manifestBlob = make([]byte, 4+len(encryptedManifest))
		binary.LittleEndian.PutUint32(manifestBlob[0:4], uint32(len(encryptedManifest)))
		copy(manifestBlob[4:], encryptedManifest)
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
					setWriteErr(fmt.Errorf("erro escrevendo frame %d no FFmpeg: %w", written, err))
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

	// 1. Write Manifest frames first (always at frames 0..manifestFrames-1 for instant Manifest-First peek!)
	for off := 0; off < len(manifestBlob); off += capPerFrame {
		end := off + capPerFrame
		if end > len(manifestBlob) {
			end = len(manifestBlob)
		}
		if err := writeChunk(manifestBlob[off:end], true, dataFrames == 0 && end >= len(manifestBlob)); err != nil {
			_ = finishPipeline()
			_ = stdin.Close()
			_ = cmd.Wait()
			return nil, err
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
					_ = finishPipeline()
					_ = stdin.Close()
					_ = cmd.Wait()
					return nil, err
				}
			}
		}
		if err := writeChunk(encryptedPayload[off:end], false, dataChunkIdx == dataFrames-1); err != nil {
			_ = finishPipeline()
			_ = stdin.Close()
			_ = cmd.Wait()
			return nil, err
		}
		dataChunkIdx++
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
				_ = finishPipeline()
				_ = stdin.Close()
				_ = cmd.Wait()
				return nil, err
			}
		} else {
			if err := writeVisualCleanFrame(); err != nil {
				_ = finishPipeline()
				_ = stdin.Close()
				_ = cmd.Wait()
				return nil, err
			}
		}
	}

	if err := finishPipeline(); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, err
	}

	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("FFmpeg encodificação falhou: %v — %s", err, strings.TrimSpace(stderr.String()))
	}

	fi, err := os.Stat(opts.OutputPath)
	var size int64
	if err == nil {
		size = fi.Size()
	}

	return &VideoEncodeStats{
		OutputPath:      opts.OutputPath,
		Width:           res.Width,
		Height:          res.Height,
		FPS:             fps,
		ProfileCode:     prof.Code,
		VideoCodec:      vCodec,
		ManifestFrames:  manifestFrames,
		DataFrames:      dataFrames,
		TotalFrames:     totalFrames,
		VideoSizeBytes:  size,
		PayloadPerFrame: capPerFrame,
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
	EncryptedManifest   []byte
	EncryptedPayload    []byte
}

// DecodeVideoStream decodes an optical video via FFmpeg pipe.
// If manifestOnly is true, stops immediately as soon as all Manifest frames (FlagManifest) are recovered!
// Automatically retries with 8K rescale if YouTube only served a 4K version of an 8K-encoded video.
func DecodeVideoStream(ffmpegPath, ffprobePath, videoPath string, manifestOnly bool, onProgress func(frame, total int)) (*VideoDecodeResult, error) {
	w, h, err := ProbeVideoDimensions(ffprobePath, ffmpegPath, videoPath)
	if err != nil {
		return nil, err
	}
	res, err := decodeVideoAtSize(ffmpegPath, videoPath, w, h, false, manifestOnly, onProgress)
	if err != nil && w == 3840 && h == 2160 {
		// Fallback: video might have been encoded in 8K and downscaled to 4K by YouTube!
		if res8k, err8k := decodeVideoAtSize(ffmpegPath, videoPath, 7680, 4320, true, manifestOnly, onProgress); err8k == nil {
			return res8k, nil
		}
	}
	return res, err
}

func decodeVideoAtSize(ffmpegPath, videoPath string, w, h int, rescale bool, manifestOnly bool, onProgress func(frame, total int)) (*VideoDecodeResult, error) {
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
		totalFrames    uint32
		containerID    uint64
		profileCode    string
		totalCorrected int
		totalRaw       int
		manifestDone   bool
		manifestSpread bool
		spreadStride   uint32
		manifestCount  uint32
		rawFrameNum    uint32
		baseHint       DecodeHint
		phaseOffset    int
	)

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
		return !manifestOnly && totalFrames > 0 && uint32(len(manifestSeen)+len(seen)) >= totalFrames
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

		// Launch multi-core parallel worker pool as soon as we know the header format (and manifestCount if spread)!
		if !manifestOnly && totalFrames > 2 && (!manifestSpread || manifestCount > 0) {
			readyForParallel = true
			break
		}
	}

	// Phase 2: Multi-core bounded parallel frame decoding across allowed CPU cores & RAM budget!
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

	if manifestDone || (totalFrames > 0 && uint32(len(manifestSeen)+len(seen)) >= totalFrames) {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()

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

	var encPayload []byte
	if !manifestOnly {
		var payloadBuf bytes.Buffer
		expectedData := int(totalFrames) - len(manifestSeen)
		if expectedData > 0 {
			if manifestSpread {
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
			} else {
				startIdx := uint32(len(manifestSeen))
				for i := startIdx; i < totalFrames; i++ {
					chunk, ok := seen[i]
					if !ok {
						return nil, fmt.Errorf("frame de dados %d/%d ausente ou corrompido", i, totalFrames)
					}
					payloadBuf.Write(chunk)
				}
			}
		}
		encPayload = payloadBuf.Bytes()
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
		EncryptedManifest:   encManifest,
		EncryptedPayload:    encPayload,
	}, nil
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

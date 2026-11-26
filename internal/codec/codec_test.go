package codec

import (
	"bytes"
	"fmt"
	"math/rand"
	"os/exec"
	"path/filepath"
	"testing"

	"ytcloud/internal/media"
)

func TestReedSolomonErrorCorrection(t *testing.T) {
	rs := GetRSCodec(179) // 76 parity bytes => corrects up to 38 arbitrary byte errors per 255-byte block!
	msg := make([]byte, 179)
	rng := rand.New(rand.NewSource(42))
	rng.Read(msg)

	cw := make([]byte, 255)
	rs.EncodeBlock(msg, cw)

	// Corrupt 35 arbitrary unknown positions (out of 255 bytes!)
	corrupted := make([]byte, 255)
	copy(corrupted, cw)
	perm := rng.Perm(255)
	for i := 0; i < 35; i++ {
		corrupted[perm[i]] ^= byte(1 + rng.Intn(254))
	}

	nErr, err := rs.DecodeBlock(corrupted)
	if err != nil {
		t.Fatalf("RS decode failed on 35 errors: %v", err)
	}
	if nErr != 35 {
		t.Fatalf("expected 35 corrected bytes, got %d", nErr)
	}
	if !bytes.Equal(corrupted[:179], msg) {
		t.Fatalf("recovered message mismatch")
	}
}

func TestOpticalFrameRoundTripWithNoiseAllProfiles(t *testing.T) {
	rng := rand.New(rand.NewSource(12345))
	w, h := 1920, 1080 // fast in-memory test across all 6 modulation profiles!
	frame := NewYUVFrame(w, h)

	for _, prof := range ListProfiles() {
		capBytes := prof.PayloadCapacity(w, h)
		payload := make([]byte, capBytes)
		rng.Read(payload)

		hdr := FrameHeader{
			ContainerID: 0x1122334455667788,
			FrameIndex:  7,
			TotalFrames: 42,
			Flags:       FlagManifest,
		}
		if err := EncodeFrame(frame, prof, hdr, payload); err != nil {
			t.Fatalf("EncodeFrame(%s) failed: %v", prof.Code, err)
		}

		// Inject simulated lossy video compression noise (+/- 10 luma/chroma jitter + 1% localized corruption)
		for i := range frame.Buf {
			jitter := rng.Intn(17) - 8 // [-8..+8]
			v := int(frame.Buf[i]) + jitter
			if v < 0 {
				v = 0
			} else if v > 255 {
				v = 255
			}
			frame.Buf[i] = byte(v)
		}

		dec, err := DecodeFrame(frame)
		if err != nil {
			t.Fatalf("DecodeFrame(%s) failed under noise: %v", prof.Code, err)
		}
		if dec.Header.FrameIndex != 7 || dec.Header.TotalFrames != 42 || dec.Header.ContainerID != hdr.ContainerID {
			t.Fatalf("Header mismatch for %s: %+v", prof.Code, dec.Header)
		}
		if !bytes.Equal(dec.Payload, payload) {
			t.Fatalf("Payload mismatch for %s", prof.Code)
		}
		t.Logf("Profile %-12s | 1080p cap: %6d B | 8K cap: %7d B | corrected under noise: %d B",
			prof.Code, capBytes, prof.PayloadCapacity(7680, 4320), dec.CorrectedBytes)
	}
}

func Test8KChunkedFFmpegPipeRoundTrip(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test_8k_chunked.mp4")

	manifest := []byte(`{"test":"8k-chunked-pipe","ok":true}`)
	payload := make([]byte, 400*1024) // 400 KB -> spans multiple 8K frames (47.5 MB each!)
	rng := rand.New(rand.NewSource(999))
	rng.Read(payload)

	stats, err := EncodeStreamToVideo(VideoEncodeOptions{
		FFmpegPath:     tools.FFmpegPath,
		OutputPath:     outPath,
		ResolutionCode: "8k",
		ProfileCode:    "y4-gray",
		FPS:            10,
		CRF:            30,
		Threads:        2,
		LowPriority:    true,
		ContainerID:    0x88776655,
	}, manifest, payload)
	if err != nil {
		t.Fatalf("8K EncodeStreamToVideo failed (chunked pipe bug?): %v", err)
	}
	t.Logf("8K encoded: %d frames, %d bytes", stats.TotalFrames, stats.VideoSizeBytes)

	dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, false, nil)
	if err != nil {
		t.Fatalf("8K DecodeVideoStream failed: %v", err)
	}
	if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
		t.Fatalf("8K round-trip payload mismatch!")
	}
}

func Test4KY8AndY6GrayMediumRoundTrip(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	manifest := []byte(`{"test":"4k-y8-y6-medium","ok":true}`)
	payload := make([]byte, 80*1024)
	rng := rand.New(rand.NewSource(2026))
	rng.Read(payload)

	for _, profCode := range []string{"y8-gray", "y6-gray", "art-stego", "art-chroma"} {
		outPath := filepath.Join(dir, "test_4k_"+profCode+".mp4")
		stats, err := EncodeStreamToVideo(VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outPath,
			ResolutionCode: "4k",
			ProfileCode:    profCode,
			FPS:            10,
			CRF:            0, // auto optimal DefaultCRF per profile
			Threads:        2,
			LowPriority:    true,
			ContainerID:    0x778899AA,
		}, manifest, payload)
		if err != nil {
			t.Fatalf("4K %s EncodeStreamToVideo failed: %v", profCode, err)
		}
		dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, false, nil)
		if err != nil {
			t.Fatalf("4K %s DecodeVideoStream failed: %v", profCode, err)
		}
		if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
			t.Fatalf("4K %s round-trip payload mismatch!", profCode)
		}
		t.Logf("4K %-8s | frames=%d | size=%d B | corrected=%d B", profCode, stats.TotalFrames, stats.VideoSizeBytes, dec.TotalCorrectedBytes)
	}
}

func TestSynthAmbientMusicHighBitSeeds(t *testing.T) {
	// Regression test: uint64 seeds with bit 63 set must never panic on modulo indexing!
	seeds := []uint64{0, 1, 0x8000000000000000, 0xFFFFFFFFFFFFFFFF, 0xCAFEBABEDEADBEEF, 0x9E3779B97F4A7C15}
	for _, s := range seeds {
		wav := SynthAmbientMusicWAV(2.0, s)
		if len(wav) < 44+44100*4 {
			t.Fatalf("unexpected short wav for seed %x: %d", s, len(wav))
		}
	}
}

func TestAllWinampArtThemesRoundTrip(t *testing.T) {
	prof := Profiles[ProfileArtStego]
	w, h := 1920, 1080
	frame := NewYUVFrame(w, h)
	capB := prof.PayloadCapacity(w, h)
	payload := make([]byte, capB)
	rng := rand.New(rand.NewSource(777))
	rng.Read(payload)

	for _, th := range ListArtThemes() {
		resolved, seed := ResolveArtTheme(th.Code, "praia", 0x9E3779B97F4A7C15)
		hdr := FrameHeader{
			ContainerID: seed,
			ArtTheme:    resolved,
			FrameIndex:  3,
			TotalFrames: 10,
		}
		if err := EncodeFrame(frame, prof, hdr, payload); err != nil {
			t.Fatalf("EncodeFrame(theme=%s) failed: %v", th.Code, err)
		}
		// Inject +/- 6 jitter to verify +/- 12 subtractive cancellation margin
		for i := range frame.Buf {
			v := int(frame.Buf[i]) + (rng.Intn(13) - 6)
			if v < 0 {
				v = 0
			} else if v > 255 {
				v = 255
			}
			frame.Buf[i] = byte(v)
		}
		dec, err := DecodeFrame(frame)
		if err != nil {
			t.Fatalf("DecodeFrame(theme=%s) failed: %v", th.Code, err)
		}
		if dec.Header.Version != 2 || dec.Header.ArtTheme != resolved || !bytes.Equal(dec.Payload, payload) {
			t.Fatalf("Round-trip mismatch for theme=%s", th.Code)
		}
		t.Logf("Theme %-10s (id=%d) | V2 1080p cap: %d B | corrected: %d B", th.Code, resolved, capB, dec.CorrectedBytes)
	}
}

func Test2KAnd720pV2FFmpegRoundTrip(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	manifest := []byte(`{"test":"2k-720p-v2","ok":true}`)
	payload := make([]byte, 64*1024)
	rng := rand.New(rand.NewSource(3030))
	rng.Read(payload)

	cases := []struct {
		res  string
		prof string
		fps  int
	}{
		{"2k", "y8-gray", 15},
		{"720p", "y4-gray", 30},
		{"720p", "y6-gray", 20},
	}
	for _, tc := range cases {
		outPath := filepath.Join(dir, tc.res+"_"+tc.prof+".mp4")
		stats, err := EncodeStreamToVideo(VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outPath,
			ResolutionCode: tc.res,
			ProfileCode:    tc.prof,
			FPS:            tc.fps,
			CRF:            0,
			Threads:        2,
			LowPriority:    true,
			ContainerID:    0x9988776655443322,
		}, manifest, payload)
		if err != nil {
			t.Fatalf("%s/%s encode failed: %v", tc.res, tc.prof, err)
		}
		dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, false, nil)
		if err != nil {
			t.Fatalf("%s/%s decode failed: %v", tc.res, tc.prof, err)
		}
		if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
			t.Fatalf("%s/%s payload mismatch", tc.res, tc.prof)
		}
		t.Logf("%-4s %-8s %2dfps | codec=%-4s | frames=%2d | size=%6d B | corrected=%d B",
			tc.res, tc.prof, tc.fps, stats.VideoCodec, stats.TotalFrames, stats.VideoSizeBytes, dec.TotalCorrectedBytes)
	}
}

func TestAV1AndFractalThemeFFmpegRoundTrip(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	manifest := []byte(`{"test":"av1-fractal","ok":true}`)
	payload := make([]byte, 32*1024)
	rng := rand.New(rand.NewSource(4040))
	rng.Read(payload)

	cases := []struct {
		name      string
		res, prof string
		vcodec    string
		audioMode string
		fps       int
		theme     uint8
	}{
		{"2K y8 auto(AV1)", "2k", "y8-gray", "auto", "none", 15, 0},
		{"720p y4 av1-mkv", "720p", "y4-gray", "av1-mkv", "none", 30, 0},
		{"720p y4 vp9-webm", "720p", "y4-gray", "vp9-webm", "none", 30, 0},
		{"720p y4 h264", "720p", "y4-gray", "h264-mp4", "none", 30, 0},
		{"720p fractal art", "720p", "art-stego", "auto", "ambient", 15, ArtThemeFractal},
		{"1080p video-fractal", "1080p", "video-fractal", "auto", "ambient", 15, ArtThemeFractal},
		{"1080p video-lightning", "1080p", "video-fractal", "auto", "none", 15, ArtThemeLightning},
		{"1080p video-beach", "1080p", "video-fractal", "auto", "none", 15, ArtThemeBeach},
		{"1080p video-chroma", "1080p", "video-chroma", "auto", "none", 15, ArtThemeBeach},
	}
	for i, tc := range cases {
		pObj, _ := ProfileByCode(tc.prof)
		ext := OutputExtensionForCodec(tc.vcodec, pObj, tools.FFmpegPath)
		outPath := filepath.Join(dir, fmt.Sprintf("af_%d%s", i, ext))
		stats, err := EncodeStreamToVideo(VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outPath,
			ResolutionCode: tc.res,
			ProfileCode:    tc.prof,
			VideoCodec:     tc.vcodec,
			AudioMode:      tc.audioMode,
			FPS:            tc.fps,
			Threads:        2,
			LowPriority:    true,
			ContainerID:    0x1234ABCD5678 + uint64(i),
			ArtTheme:       tc.theme,
		}, manifest, payload)
		if err != nil {
			t.Fatalf("%s encode failed: %v", tc.name, err)
		}
		dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, false, nil)
		if err != nil {
			t.Fatalf("%s decode failed: %v", tc.name, err)
		}
		if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
			t.Fatalf("%s payload mismatch", tc.name)
		}
		t.Logf("%-20s | %-4s %-5s | frames=%2d | size=%6d B | corrected=%d B",
			tc.name, stats.VideoCodec, ext, stats.TotalFrames, stats.VideoSizeBytes, dec.TotalCorrectedBytes)
	}
}

func TestMandatoryTargetFramesTrailingVisualRoundTrip(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	manifest := []byte(`{"test":"mandatory-duration","ok":true}`)
	payload := make([]byte, 8*1024)
	rng := rand.New(rand.NewSource(9090))
	rng.Read(payload)

	outPath := filepath.Join(dir, "padded_fractal.mp4")
	// 8 KB in 720p art-stego needs ~9 data frames; force TargetFrames = 30 (2.0s @ 15fps)
	stats, err := EncodeStreamToVideo(VideoEncodeOptions{
		FFmpegPath:     tools.FFmpegPath,
		OutputPath:     outPath,
		ResolutionCode: "720p",
		ProfileCode:    "art-stego",
		VideoCodec:     "auto",
		AudioMode:      "ambient",
		FPS:            15,
		TargetFrames:   30,
		Threads:        2,
		LowPriority:    true,
		ContainerID:    0x7777AAAA1111,
		ArtTheme:       ArtThemeFractal,
	}, manifest, payload)
	if err != nil {
		t.Fatalf("padded encode failed: %v", err)
	}
	dur, err := ProbeMediaDuration(tools.FFprobePath, tools.FFmpegPath, outPath)
	if err != nil || dur < 1.8 {
		t.Fatalf("expected ~2.0s mandatory duration, got %.2fs (err=%v)", dur, err)
	}
	dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, false, nil)
	if err != nil {
		t.Fatalf("padded decode failed: %v", err)
	}
	if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
		t.Fatalf("padded payload mismatch")
	}
	t.Logf("Mandatory TargetFrames OK | dataFrames=%d | dur=%.2fs | size=%d B | corrected=%d B",
		stats.TotalFrames, dur, stats.VideoSizeBytes, dec.TotalCorrectedBytes)
}

func TestMaskIntensityAndHiddenHeaderFFmpegRoundTrip(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	manifest := []byte(`{"test":"intensity-hidden-hdr","ok":true}`)
	payload := make([]byte, 12*1024)
	rng := rand.New(rand.NewSource(7777))
	rng.Read(payload)

	cases := []struct {
		name      string
		res, prof string
		theme     uint8
		intensity uint8
		hideHdr   bool
	}{
		{"art-stego subtle+hidden", "720p", "art-stego", ArtThemeFractal, MaskIntensitySubtle, true},
		{"art-stego balanced+hidden", "720p", "art-stego", ArtThemeWinamp, MaskIntensityDefault, true},
		{"art-stego strong(2x)+hidden", "1080p", "art-stego", ArtThemeBeach, MaskIntensityStrong, true},
		{"art-stego max(4x)+hidden", "720p", "art-stego", ArtThemeLightning, MaskIntensityMax, true},
		{"video-fractal subtle+hidden", "1080p", "video-fractal", ArtThemeFractal, MaskIntensitySubtle, true},
		{"video-fractal strong(2x)+hidden", "1080p", "video-fractal", ArtThemeAurora, MaskIntensityStrong, true},
		{"video-fractal max(4x)+hidden", "1080p", "video-fractal", ArtThemeFlora, MaskIntensityMax, true},
		{"video-chroma strong(2x)+hidden", "1080p", "video-chroma", ArtThemeBeach, MaskIntensityStrong, true},
	}

	for i, tc := range cases {
		outPath := filepath.Join(dir, fmt.Sprintf("ih_%d.mp4", i))
		stats, err := EncodeStreamToVideo(VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outPath,
			ResolutionCode: tc.res,
			ProfileCode:    tc.prof,
			VideoCodec:     "h264-mp4",
			AudioMode:      "none",
			FPS:            15,
			Threads:        2,
			LowPriority:    true,
			ContainerID:    0xFACE0000 + uint64(i),
			ArtTheme:       tc.theme,
			MaskIntensity:  tc.intensity,
			HideHeaderBar:  tc.hideHdr,
		}, manifest, payload)
		if err != nil {
			t.Fatalf("%s encode failed: %v", tc.name, err)
		}
		dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, false, nil)
		if err != nil {
			t.Fatalf("%s decode failed: %v", tc.name, err)
		}
		if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
			t.Fatalf("%s payload mismatch", tc.name)
		}
		t.Logf("%-32s | cap=%5d B/f | frames=%2d | size=%6d B | corrected=%d B",
			tc.name, stats.PayloadPerFrame, stats.TotalFrames, stats.VideoSizeBytes, dec.TotalCorrectedBytes)
	}
}

func TestHostVideoOnAllMaskModesAndExactCut(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	// Create a synthetic 6.0-second host video WITH audio to verify:
	// 1. Exact cutting when DurationSource=="files" (TargetFrames==0) cuts 6s host down to ~0.6s!
	// 2. Host video works blindly on art-stego (FlagHostManchester), art-chroma, AND video-fractal!
	hostPath := filepath.Join(dir, "host_6s.mp4")
	cmd := exec.Command(tools.FFmpegPath,
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=15:duration=6",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", hostPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create synthetic 6s host video: %v %s", err, string(out))
	}

	manifest := []byte(`{"test":"host-cut-all-masks","ok":true}`)
	payload := make([]byte, 4*1024) // small 4 KB payload -> ~5 frames (~0.33s @ 15fps), MUST cut 6s host!
	rand.New(rand.NewSource(6060)).Read(payload)

	for i, profCode := range []string{"art-stego", "art-chroma", "video-fractal", "video-bw"} {
		outPath := filepath.Join(dir, fmt.Sprintf("cut_%d_%s.mp4", i, profCode))
		stats, err := EncodeStreamToVideo(VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outPath,
			ResolutionCode: "720p",
			ProfileCode:    profCode,
			VideoCodec:     "h264-mp4",
			AudioMode:      "host",
			HostVideoPath:  hostPath,
			FPS:            15,
			TargetFrames:   0, // Mandatory = files -> MUST cut 6.0s host down to data duration!
			Threads:        2,
			LowPriority:    true,
			ContainerID:    0x88880000 + uint64(i),
			ArtTheme:       ArtThemeFractal,
			HideHeaderBar:  profCode != "art-chroma",
		}, manifest, payload)
		if err != nil {
			t.Fatalf("%s encode with host failed: %v", profCode, err)
		}
		dur, err := ProbeMediaDuration(tools.FFprobePath, tools.FFmpegPath, outPath)
		if err != nil {
			t.Fatalf("%s probe dur: %v", profCode, err)
		}
		if dur > 2.0 {
			t.Fatalf("%s did NOT cut 6s host video! got %.2fs (frames=%d)", profCode, dur, stats.TotalFrames)
		}
		dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, false, nil)
		if err != nil {
			t.Fatalf("%s blind decode failed: %v", profCode, err)
		}
		if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
			t.Fatalf("%s payload mismatch", profCode)
		}
		t.Logf("%-14s + 6s host | cut dur=%.2fs (frames=%2d) | size=%6d B | corrected=%d B",
			profCode, dur, stats.TotalFrames, stats.VideoSizeBytes, dec.TotalCorrectedBytes)
	}
}

func TestNewGeometricArtProfilesFFmpegRoundTrip(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	manifest := []byte(`{"test":"geometric-art","ok":true}`)
	payload := make([]byte, 32*1024)
	rand.New(rand.NewSource(9090)).Read(payload)

	profs := []struct {
		code      string
		intensity uint8
		hideHdr   bool
	}{
		{"art-crystal", MaskIntensityDefault, true},
		{"art-crystal", MaskIntensitySubtle, true},
		{"art-fractal3d", MaskIntensityDefault, true},
		{"art-angles", MaskIntensityDefault, true},
		{"art-fractal-angles", MaskIntensityDefault, true},
		{"art-bauhaus", MaskIntensityDefault, true},
		{"art-constel", MaskIntensityDefault, true},
		{"art-truchet", MaskIntensityDefault, true},
		{"art-truchet", MaskIntensityDefault, false},
		{"art-fullres", MaskIntensityDefault, true},
		{"art-fullres", MaskIntensitySubtle, true},
	}

	for i, tc := range profs {
		outPath := filepath.Join(dir, fmt.Sprintf("geo_%d_%s.mp4", i, tc.code))
		stats, err := EncodeStreamToVideo(VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outPath,
			ResolutionCode: "1080p",
			ProfileCode:    tc.code,
			VideoCodec:     "auto",
			AudioMode:      "none",
			FPS:            15,
			Threads:        2,
			LowPriority:    true,
			ContainerID:    0x770000 + uint64(i),
			ArtTheme:       ArtThemeFractal,
			MaskIntensity:  tc.intensity,
			HideHeaderBar:  tc.hideHdr,
		}, manifest, payload)
		if err != nil {
			t.Fatalf("%s encode failed: %v", tc.code, err)
		}
		dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, false, nil)
		if err != nil {
			t.Fatalf("%s decode failed: %v", tc.code, err)
		}
		if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
			t.Fatalf("%s payload mismatch", tc.code)
		}
		ratio := float64(stats.VideoSizeBytes) / float64(len(payload))
		t.Logf("%-18s | codec=%-4s | cap=%5d B/f | frm=%2d | ratio=%5.2fx | corrected=%d B",
			tc.code, stats.VideoCodec, stats.PayloadPerFrame, stats.TotalFrames, ratio, dec.TotalCorrectedBytes)
	}
}

func TestPaddingModesAutoSubtleAndSpreadFFmpegRoundTrip(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	manifest := []byte(`{"test":"padding-modes","ok":true}`)
	payload := make([]byte, 12*1024)
	rand.New(rand.NewSource(4242)).Read(payload)

	for _, mode := range []string{"auto", "spread", "start"} {
		outPath := filepath.Join(dir, "pad_"+mode+".mp4")
		stats, err := EncodeStreamToVideo(VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outPath,
			ResolutionCode: "720p",
			ProfileCode:    "art-fullres",
			VideoCodec:     "h264-mp4",
			AudioMode:      "none",
			FPS:            15,
			Threads:        2,
			LowPriority:    true,
			ContainerID:    0x99887766,
			ArtTheme:       ArtThemeJuliaTuring,
			HideHeaderBar:  true,
			TargetFrames:   45, // 3.0s mandatory duration > payload needed frames!
			PaddingMode:    mode,
		}, manifest, payload)
		if err != nil {
			t.Fatalf("mode=%s encode failed: %v", mode, err)
		}
		// Verify Manifest-First Peek works cleanly in all 3 modes
		peek, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, true, nil)
		if err != nil || !bytes.Equal(peek.EncryptedManifest, manifest) {
			t.Fatalf("mode=%s manifest peek failed: err=%v", mode, err)
		}
		// Verify Full Decode recovers exact payload
		dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, false, nil)
		if err != nil {
			t.Fatalf("mode=%s full decode failed: %v", mode, err)
		}
		if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
			t.Fatalf("mode=%s payload mismatch", mode)
		}
		t.Logf("PaddingMode=%-6s | dataFrames=%2d | cap=%4d B/f | size=%6d B | corrected=%d B",
			mode, stats.TotalFrames, stats.PayloadPerFrame, stats.VideoSizeBytes, dec.TotalCorrectedBytes)
	}
}

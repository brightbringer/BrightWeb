package codec

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCrossFrameAndPartitioned4xProductionEndToEnd(t *testing.T) {
	ffmpegPath := filepath.Join(os.Getenv("LOCALAPPDATA"), "YTCloud", "bin", "ffmpeg.exe")
	ffprobePath := filepath.Join(os.Getenv("LOCALAPPDATA"), "YTCloud", "bin", "ffprobe.exe")
	if _, err := os.Stat(ffmpegPath); err != nil {
		ffmpegPath = "ffmpeg"
		ffprobePath = "ffprobe"
		if _, err := exec.LookPath(ffmpegPath); err != nil {
			t.Skip("ffmpeg not found")
		}
	}

	tmpDir := t.TempDir()
	prof := Profiles[ProfileY4Gray]
	res := Resolutions["720p"]
	capPerFrame := prof.PayloadCapacityLayout(res.Width, res.Height, true)

	// Deliberately choose an ODD, non-multiple-of-6 frame count (100 frames + partial tail 123 bytes -> 101 data frames!)
	// to prove virtual-zero padding & exact OrigPayloadLen trimming work bit-for-bit!
	payloadLen := 100*capPerFrame + 123
	payload := make([]byte, payloadLen)
	_, _ = rand.Read(payload)
	origHash := sha256.Sum256(payload)
	manifest := []byte(`{"test":"crossframe_ytcf_v1","odd_frames":101}`)

	// 1. Encode with Cross-Frame RS (K=6, M=2) + VP9 -> triggers BOTH Cross-Frame YTCF AND 4x Partitioned Encode!
	vp9Path := filepath.Join(tmpDir, "shield_vp9.webm")
	st, err := EncodeStreamToVideo(VideoEncodeOptions{
		FFmpegPath:     ffmpegPath,
		OutputPath:     vp9Path,
		ResolutionCode: "720p",
		ProfileCode:    "y4-gray",
		VideoCodec:     "vp9",
		FPS:            15,
		Threads:        8,
		ContainerID:    0x77770001,
		CrossFECMode:   "max", // K=6, M=2
	}, manifest, payload)
	if err != nil {
		t.Fatalf("EncodeStreamToVideo VP9+CrossFrame failed: %v", err)
	}
	t.Logf("VP9+CrossFrame encoded: total=%d (manifest=%d data=%d parity=%d K=%d M=%d) partitionedEncode=%v size=%.2fMB",
		st.TotalFrames, st.ManifestFrames, st.DataFrames, st.CrossFrameParityFrames, st.CrossFrameK, st.CrossFrameM,
		st.PartitionedEncode, float64(st.VideoSizeBytes)/(1024*1024))
	if st.DataFrames != 101 || st.CrossFrameK != 6 || st.CrossFrameM != 2 || st.CrossFrameParityFrames != 34 {
		t.Fatalf("unexpected CrossFrame stats: data=%d K=%d M=%d parity=%d", st.DataFrames, st.CrossFrameK, st.CrossFrameM, st.CrossFrameParityFrames)
	}
	if !st.PartitionedEncode {
		t.Errorf("expected PartitionedEncode=true for 136-frame VP9 video")
	}

	// 2. Decode intact video -> should trigger 4x Partitioned Decode AND match SHA-256 bit-for-bit!
	dec, err := DecodeVideoStream(ffmpegPath, ffprobePath, vp9Path, false, nil)
	if err != nil {
		t.Fatalf("DecodeVideoStream intact failed: %v", err)
	}
	t.Logf("Intact decode: frames=%d/%d partitionedDecode=%v crossFrameRecovered=%d",
		dec.DecodedFrames, dec.TotalFrames, dec.PartitionedDecode, dec.CrossFrameRecovered)
	if !bytes.Equal(dec.EncryptedManifest, manifest) {
		t.Fatalf("manifest mismatch")
	}
	if len(dec.EncryptedPayload) != len(payload) || sha256.Sum256(dec.EncryptedPayload) != origHash {
		t.Fatalf("payload mismatch on intact decode (got len %d, want %d)", len(dec.EncryptedPayload), len(payload))
	}
	if !dec.PartitionedDecode {
		t.Errorf("expected PartitionedDecode=true for >=120 frame video")
	}

	// 3. REAL BLACKOUT ATTACK: physically paint 26 consecutive frames (frames 40..65 = ~19% of video!) SOLID BLACK!
	blackoutPath := filepath.Join(tmpDir, "shield_blackout.mp4")
	blkCmd := exec.Command(ffmpegPath,
		"-y", "-loglevel", "error",
		"-i", vp9Path,
		"-vf", "drawbox=enable='between(n,40,65)':x=0:y=0:w=iw:h=ih:color=black@1.0:t=fill",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "24", "-pix_fmt", "yuv420p",
		blackoutPath,
	)
	if out, err := blkCmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg blackout filter failed: %v (%s)", err, string(out))
	}

	decBlk, err := DecodeVideoStream(ffmpegPath, ffprobePath, blackoutPath, false, nil)
	if err != nil {
		t.Fatalf("DecodeVideoStream under 26-frame REAL BLACKOUT failed: %v", err)
	}
	if len(decBlk.EncryptedPayload) != len(payload) || sha256.Sum256(decBlk.EncryptedPayload) != origHash {
		t.Fatalf("SHA-256 mismatch after blackout reconstruction!")
	}
	if decBlk.CrossFrameRecovered < 20 {
		t.Fatalf("expected >=20 frames reconstructed by Cross-Frame RS, got %d", decBlk.CrossFrameRecovered)
	}
	t.Logf("REAL 26-FRAME BLACKOUT SURVIVED 100%%!! Reconstructed %d completely blacked-out frames with exact SHA-256 match!", decBlk.CrossFrameRecovered)
}

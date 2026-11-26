package codec

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"ytcloud/internal/media"
)

func TestStealthProfilesProductionRoundTrip(t *testing.T) {
	const (
		w, h    = 1280, 720
		nFrames = 8
	)
	steamboat := loadSteamboatFramesForLab(w, h)
	tools := media.DetectTools()

	profiles := []ProfileID{
		ProfileStealthVHS,
		ProfileStealthRain,
		ProfileStealthFilm,
		ProfileStealthStars,
	}

	for _, pid := range profiles {
		prof := Profiles[pid]
		for _, hideHdr := range []bool{true, false} {
			flags := BuildHeaderFlags(MaskIntensityDefault, hideHdr)
			capB := prof.PayloadCapacityLayoutWithFlags(w, h, true, flags)
			if capB < 179 {
				t.Fatalf("%s (hideHdr=%v): expected >= 179 B/frame in 720p, got %d", prof.Code, hideHdr, capB)
			}

			encFrames := make([]*YUVFrame, nFrames)
			payloads := make([][]byte, nFrames)
			rng := rand.New(rand.NewSource(int64(4242 + int(pid)*10)))

			for fi := 0; fi < nFrames; fi++ {
				var host *YUVFrame
				if len(steamboat) > 0 {
					host = buildScenicColorHost(w, h, fi, steamboat[fi%len(steamboat)])
				}
				pl := make([]byte, capB)
				rng.Read(pl)
				payloads[fi] = pl

				dst := NewYUVFrame(w, h)
				hdr := FrameHeader{
					ContainerID: 0xABCDEF1234567890,
					ArtTheme:    ArtThemeNone,
					Flags:       flags,
					FrameIndex:  uint32(fi),
					TotalFrames: uint32(nFrames),
				}
				if err := EncodeFrameWithHost(dst, host, prof, hdr, pl); err != nil {
					t.Fatalf("%s (hideHdr=%v) encode frame %d: %v", prof.Code, hideHdr, fi, err)
				}
				encFrames[fi] = dst

				// 1. Direct lossless decode verification
				res, err := DecodeFrame(dst)
				if err != nil {
					t.Fatalf("%s (hideHdr=%v) direct decode frame %d: %v", prof.Code, hideHdr, fi, err)
				}
				if res.Header.ProfileID != pid || res.Header.FrameIndex != uint32(fi) {
					t.Fatalf("%s (hideHdr=%v) header mismatch: got prof=%d idx=%d", prof.Code, hideHdr, res.Header.ProfileID, res.Header.FrameIndex)
				}
				if !bytes.Equal(res.Payload, pl) {
					t.Fatalf("%s (hideHdr=%v) direct payload mismatch on frame %d", prof.Code, hideHdr, fi)
				}
			}

			// 2. Verify FlagVisualFiller frames are cleanly rejected by DecodeFrame (not confused with data frames)
			filler := NewYUVFrame(w, h)
			fillerHdr := FrameHeader{
				ContainerID: 0xABCDEF1234567890,
				ArtTheme:    ArtThemeNone,
				Flags:       flags | FlagVisualFiller,
				FrameIndex:  99,
				TotalFrames: uint32(nFrames),
			}
			if err := EncodeFrameWithHost(filler, nil, prof, fillerHdr, payloads[0]); err != nil {
				t.Fatalf("%s filler encode: %v", prof.Code, err)
			}
			if _, err := DecodeFrame(filler); err == nil {
				t.Fatalf("%s (hideHdr=%v): expected visual filler frame to be skipped by DecodeFrame", prof.Code, hideHdr)
			}

			// 3. Real FFmpeg H.264 (CRF 23) + VP9 (CRF 34, YouTube-style) round-trip verification for full-screen hideHdr=true!
			if hideHdr && tools.HasFFmpeg {
				for _, tc := range []struct {
					name string
					crf  int
				}{
					{"h264", 23},
					{"vp9", 34},
				} {
					recFrames, sizeB := transcodeSingleRoundTrip(t, tools.FFmpegPath, encFrames, tc.name, tc.crf)
					if len(recFrames) != nFrames {
						t.Fatalf("%s %s: expected %d frames, got %d", prof.Code, tc.name, nFrames, len(recFrames))
					}
					totalCorr := 0
					for fi := 0; fi < nFrames; fi++ {
						res, err := DecodeFrame(recFrames[fi])
						if err != nil {
							t.Fatalf("%s %s frame %d decode failed: %v", prof.Code, tc.name, fi, err)
						}
						if !bytes.Equal(res.Payload, payloads[fi]) {
							t.Fatalf("%s %s frame %d payload mismatch!", prof.Code, tc.name, fi)
						}
						totalCorr += res.CorrectedBytes
					}
					t.Logf("PASS %-14s | 720p hideHdr=true | %-4s(CRF%d): %d/%d frames OK (RS corrected=%d B, %.1f KB)",
						prof.Code, tc.name, tc.crf, nFrames, nFrames, totalCorr, float64(sizeB)/1024.0)
				}
			}
		}
	}
}

func TestStaticJPEGHostImageRoundTrip(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("FFmpeg não disponível")
	}
	tmpDir := t.TempDir()
	jpgPath := filepath.Join(tmpDir, "photo_host.jpg")
	img := image.NewRGBA(image.Rect(0, 0, 1600, 900))
	for y := 0; y < 900; y++ {
		for x := 0; x < 1600; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(30 + (x*140)/1600),
				G: uint8(45 + (y*120)/900),
				B: uint8(80 + ((x+y)*100)/2500),
				A: 255,
			})
		}
	}
	f, err := os.Create(jpgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	manifest := []byte(`{"site":"ytbrowser-jpg-host-test"}`)
	payload := make([]byte, 1200)
	rand.New(rand.NewSource(777)).Read(payload)
	outMP4 := filepath.Join(tmpDir, "site_from_jpg.mp4")

	stats, err := EncodeStreamToVideo(VideoEncodeOptions{
		FFmpegPath:     tools.FFmpegPath,
		OutputPath:     outMP4,
		ResolutionCode: "720p",
		ProfileCode:    "stealth-stars",
		VideoCodec:     "auto",
		FPS:            15,
		Threads:        4,
		ContainerID:    0x1122334455667788,
		ArtTheme:       ArtThemeNone,
		HideHeaderBar:  true,
		HostVideoPath:  jpgPath,
		AudioMode:      "auto",
		TargetFrames:   30,
		PaddingMode:    "auto",
	}, manifest, payload)
	if err != nil {
		t.Fatalf("EncodeStreamToVideo with JPG host failed: %v", err)
	}
	dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outMP4, false, nil)
	if err != nil {
		t.Fatalf("DecodeVideoStream failed: %v", err)
	}
	if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
		t.Fatalf("JPG host round-trip mismatch! got manifest=%d payload=%d", len(dec.EncryptedManifest), len(dec.EncryptedPayload))
	}
	t.Logf("PASS JPG host round-trip: %d frames, %.1f KB MP4, recovered %d B payload", stats.TotalFrames, float64(stats.VideoSizeBytes)/1024.0, len(dec.EncryptedPayload))
}

func TestStealthAll4OnHighContrastLogoHost(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("FFmpeg não disponível")
	}
	tmpDir := t.TempDir()
	// Use actual DOOM cover if present in scratch, else synthesize a brutal pure-white-logo + black-edge cover!
	hostImg := `C:\Users\mathe\.gemini\antigravity\brain\4587ad20-5bae-4c03-a984-8dc228b6d652\scratch\doom_frame0.png`
	if _, err := os.Stat(hostImg); err != nil {
		hostImg = filepath.Join(tmpDir, "brutal_logo.jpg")
		img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
		for y := 0; y < 720; y++ {
			for x := 0; x < 1280; x++ {
				if y < 180 && ((x/24)%2 == 0) {
					img.SetRGBA(x, y, color.RGBA{255, 255, 255, 255}) // clipped white logo bars at top!
				} else if y < 180 {
					img.SetRGBA(x, y, color.RGBA{0, 0, 0, 255}) // clipped black outlines!
				} else {
					img.SetRGBA(x, y, color.RGBA{210, 65, 25, 255})
				}
			}
		}
		f, _ := os.Create(hostImg)
		_ = jpeg.Encode(f, img, &jpeg.Options{Quality: 92})
		f.Close()
	}

	manifest := []byte(`{"site":"doom1993-stress-test"}`)
	payload := make([]byte, 1400)
	rand.New(rand.NewSource(1993)).Read(payload)

	for _, code := range []string{"stealth-vhs", "stealth-rain", "stealth-film", "stealth-stars"} {
		for _, vc := range []string{"h264", "vp9"} {
			ext := ".mp4"
			if vc == "vp9" {
				ext = ".webm"
			}
			outPath := filepath.Join(tmpDir, code+"_"+vc+ext)
			stats, err := EncodeStreamToVideo(VideoEncodeOptions{
				FFmpegPath:     tools.FFmpegPath,
				OutputPath:     outPath,
				ResolutionCode: "720p",
				ProfileCode:    code,
				VideoCodec:     vc,
				FPS:            10,
				Threads:        4,
				ContainerID:    0xD00D1993CAFEBABE,
				ArtTheme:       ArtThemeNone,
				HideHeaderBar:  true,
				HostVideoPath:  hostImg,
				AudioMode:      "none",
				TargetFrames:   0,
				PaddingMode:    "auto",
			}, manifest, payload)
			if err != nil {
				t.Fatalf("%s (%s) encode failed: %v", code, vc, err)
			}
			dec, err := DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, outPath, false, nil)
			if err != nil {
				t.Fatalf("%s (%s) decode failed: %v", code, vc, err)
			}
			if !bytes.Equal(dec.EncryptedManifest, manifest) || !bytes.Equal(dec.EncryptedPayload, payload) {
				t.Fatalf("%s (%s) round-trip mismatch!", code, vc)
			}
			t.Logf("PASS DOOM-COVER %-14s | %-4s | %d frames | %.1f KB | corrected=%d B",
				code, vc, stats.TotalFrames, float64(stats.VideoSizeBytes)/1024.0, dec.TotalCorrectedBytes)
		}
	}
}

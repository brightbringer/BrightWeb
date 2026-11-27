package server

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"ytcloud/internal/codec"
)

var (
	steamboatOnce   sync.Once
	steamboatFrames [][]byte // nFrames slices of 320x180 grayscale Y
	steamboatW      int
	steamboatH      int

	previewPalOnce sync.Once
	previewPalette color.Palette
	grayLUT        [256]uint8

	previewCacheMu sync.Mutex
	previewCache   = map[string][]byte{}
)

func loadSteamboatHost() ([][]byte, int, int) {
	steamboatOnce.Do(func() {
		f, err := webFS.Open("web/previews/steamboat_host.gif")
		if err != nil {
			return
		}
		defer f.Close()
		g, err := gif.DecodeAll(f)
		if err != nil || len(g.Image) == 0 {
			return
		}
		w := g.Config.Width
		h := g.Config.Height
		if w <= 0 || h <= 0 {
			b := g.Image[0].Bounds()
			w, h = b.Dx(), b.Dy()
		}
		frames := make([][]byte, 0, len(g.Image))
		canvas := make([]byte, w*h)
		for _, img := range g.Image {
			b := img.Bounds()
			pal := img.Palette
			for y := b.Min.Y; y < b.Max.Y && y < h; y++ {
				for x := b.Min.X; x < b.Max.X && x < w; x++ {
					ci := img.ColorIndexAt(x, y)
					r, gr, bl, a := pal[ci].RGBA()
					if a == 0 {
						continue
					}
					lum := (r*299 + gr*587 + bl*114) / 1000
					canvas[y*w+x] = byte(lum >> 8)
				}
			}
			cp := make([]byte, len(canvas))
			copy(cp, canvas)
			frames = append(frames, cp)
		}
		steamboatW, steamboatH, steamboatFrames = w, h, frames
	})
	return steamboatFrames, steamboatW, steamboatH
}

func getPreviewPalette() color.Palette {
	previewPalOnce.Do(func() {
		// 216-color 6x6x6 uniform RGB cube + 32 dedicated smooth grayscale tones = 248 colors!
		pal := make(color.Palette, 0, 248)
		for r := 0; r < 6; r++ {
			for g := 0; g < 6; g++ {
				for b := 0; b < 6; b++ {
					pal = append(pal, color.RGBA{
						R: uint8(r * 51),
						G: uint8(g * 51),
						B: uint8(b * 51),
						A: 255,
					})
				}
			}
		}
		for i := 0; i < 32; i++ {
			v := uint8(i * 255 / 31)
			pal = append(pal, color.RGBA{R: v, G: v, B: v, A: 255})
		}
		for v := 0; v < 256; v++ {
			grayLUT[v] = uint8(216 + (v*31+127)/255)
		}
		previewPalette = pal
	})
	return previewPalette
}

// upscaleGray4x expands a 320x180 grayscale slice into a 1280x720 YUVFrame Y-plane with smooth bilinear interpolation.
func upscaleGray4x(src []byte, sw, sh int, dst *codec.YUVFrame) {
	Y := dst.YPlane()
	dw, dh := dst.Width, dst.Height
	for y := 0; y < dh; y++ {
		sy := y >> 2
		sy1 := sy + 1
		if sy1 >= sh {
			sy1 = sh - 1
		}
		wy := y & 3
		iwy := 4 - wy
		sRow0 := sy * sw
		sRow1 := sy1 * sw
		dRow := y * dw
		for x := 0; x < dw; x++ {
			sx := x >> 2
			sx1 := sx + 1
			if sx1 >= sw {
				sx1 = sw - 1
			}
			wx := x & 3
			iwx := 4 - wx
			v00 := int(src[sRow0+sx])
			v10 := int(src[sRow0+sx1])
			v01 := int(src[sRow1+sx])
			v11 := int(src[sRow1+sx1])
			val := ((v00*iwx+v10*wx)*iwy + (v01*iwx+v11*wx)*wy + 8) >> 4
			Y[dRow+x] = byte(val)
		}
	}
}

var bayer2x2 = [2][2]int{
	{-12, 12},
	{4, -4},
}

// downsampleYUVToPaletted converts a 1280x720 YUV420p frame into a crisp 320x180 paletted GIF frame.
// When zoom2x is true (used for fine 8x8 geometric/angular patterns), it crops the top-center 640x360
// region at 2:1 so every 8x8 tile renders at 4x4 pixels (making lines, crystals, triangles & Truchet curves
// clearly visible while still showing the top header bar when hide_header=false!).
func downsampleYUVToPaletted(f *codec.YUVFrame, pal color.Palette) *image.Paletted {
	return downsampleYUVToPalettedMode(f, pal, false)
}

func downsampleYUVToPalettedMode(f *codec.YUVFrame, pal color.Palette, zoom2x bool) *image.Paletted {
	return downsampleYUVToPalettedSize(f, pal, 320, 180, zoom2x)
}

func downsampleYUVToPalettedSize(f *codec.YUVFrame, pal color.Palette, pw, ph int, zoom2x bool) *image.Paletted {
	img := image.NewPaletted(image.Rect(0, 0, pw, ph), pal)
	Y := f.YPlane()
	U := f.UPlane()
	V := f.VPlane()
	w := f.Width
	uvW := w / 2

	step := w / pw
	if step < 1 {
		step = 1
	}
	xOff := 0
	if zoom2x && step >= 4 && f.Width >= pw*2 && f.Height >= ph*2 {
		step = 2
		xOff = (f.Width - pw*2) / 2
	}

	for py := 0; py < ph; py++ {
		sy := py*step + step/2
		if sy >= f.Height {
			sy = f.Height - 1
		}
		cy := sy >> 1
		yOff := sy * w
		uvOff := cy * uvW
		dRow := py * pw
		bRow := bayer2x2[py&1]
		for px := 0; px < pw; px++ {
			sx := xOff + px*step + step/2
			if sx >= w {
				sx = w - 1
			}
			yv := int(Y[yOff+sx])
			uv := int(U[uvOff+(sx>>1)]) - 128
			vv := int(V[uvOff+(sx>>1)]) - 128

			// Fast studio-range BT.601 YUV -> RGB
			c := yv - 16
			if c < 0 {
				c = 0
			}
			r := (298*c + 409*vv + 128) >> 8
			g := (298*c - 100*uv - 208*vv + 128) >> 8
			b := (298*c + 516*uv + 128) >> 8

			d := bRow[px&1]
			// Use dedicated 32-step grayscale ramp for near-neutral pixels (cleaner Steamboat Willie & B&W header!)
			maxDiff := r - g
			if maxDiff < 0 {
				maxDiff = -maxDiff
			}
			dg := g - b
			if dg < 0 {
				dg = -dg
			}
			if dg > maxDiff {
				maxDiff = dg
			}
			if maxDiff <= 8 {
				lum := (r + g + b) / 3
				if lum < 0 {
					lum = 0
				} else if lum > 255 {
					lum = 255
				}
				img.Pix[dRow+px] = grayLUT[lum]
				continue
			}

			r += d
			g += d
			b += d
			if r < 0 {
				r = 0
			} else if r > 255 {
				r = 255
			}
			if g < 0 {
				g = 0
			} else if g > 255 {
				g = 255
			}
			if b < 0 {
				b = 0
			} else if b > 255 {
				b = 255
			}
			ri := (r*5 + 127) / 255
			gi := (g*5 + 127) / 255
			bi := (b*5 + 127) / 255
			img.Pix[dRow+px] = uint8(ri*36 + gi*6 + bi)
		}
	}
	return img
}

// GeneratePreviewGIF renders an authentic 10-frame animated GIF using the real YTCloud optical codec
// for any combination of (profile, theme, maskIntensity, hideHeaderBar, useHost) — including the real top header bar!
func GeneratePreviewGIF(profCode, themeCode, intensityCode string, hideHeader bool) ([]byte, error) {
	return GeneratePreviewGIFWithHost(profCode, themeCode, intensityCode, hideHeader, false)
}

func GeneratePreviewGIFWithHost(profCode, themeCode, intensityCode string, hideHeader, forceHost bool) ([]byte, error) {
	return GeneratePreviewGIFCustom(profCode, themeCode, intensityCode, hideHeader, forceHost, false, false)
}

func GeneratePreviewGIFCustom(profCode, themeCode, intensityCode string, hideHeader, forceHost, explicitHost, large bool) ([]byte, error) {
	prof, err := codec.ProfileByCode(profCode)
	if err != nil {
		prof = codec.Profiles[codec.ProfileArtStego]
		profCode = prof.Code
	}
	maskIntensity := codec.ParseMaskIntensity(intensityCode)
	var useHost bool
	if explicitHost {
		useHost = forceHost
	} else {
		useHost = forceHost || prof.ArtMode == "video-fractal-chroma" || prof.ArtMode == "video-chroma-q4" || prof.ArtMode == "video-bw" || codec.IsStealthArtMode(prof.ArtMode)
	}
	canHide := codec.CanHideHeaderArtMode(prof.ArtMode, useHost)
	if hideHeader && !canHide {
		hideHeader = false
	}
	if themeCode == "" || themeCode == "auto" {
		themeCode = "fractal"
	}

	pw, ph := 320, 180
	if large {
		pw, ph = 640, 360
	}
	cacheKey := fmt.Sprintf("%s|%s|%d|%v|%v|%dx%d", profCode, themeCode, maskIntensity, hideHeader, useHost, pw, ph)
	previewCacheMu.Lock()
	if cached, ok := previewCache[cacheKey]; ok {
		previewCacheMu.Unlock()
		return cached, nil
	}
	previewCacheMu.Unlock()

	themeID, seed := codec.ResolveArtTheme(themeCode, "", 0x1928A1B2C3D4)
	const (
		w, h    = 1280, 720
		nFrames = 10
	)
	baseFlags := codec.BuildHeaderFlags(maskIntensity, hideHeader)
	if useHost && (prof.ArtMode == "subtractive-y2" || prof.ArtMode == "video-bw") {
		baseFlags |= codec.FlagHostManchester
	}
	capB := prof.PayloadCapacityLayoutWithFlags(w, h, true, baseFlags)
	if capB <= 0 {
		return nil, fmt.Errorf("capacidade inválida para preview")
	}
	rng := rand.New(rand.NewSource(1928))
	payload := make([]byte, capB)
	rng.Read(payload)

	var hostGray [][]byte
	var sw, sh int
	if useHost {
		hostGray, sw, sh = loadSteamboatHost()
	}

	pal := getPreviewPalette()
	dst := codec.NewYUVFrame(w, h)
	var hostBuf *codec.YUVFrame
	if useHost && len(hostGray) > 0 {
		hostBuf = codec.NewYUVFrame(w, h)
		// Initialize neutral chroma in hostBuf (Steamboat Willie is B&W)
		for i := range hostBuf.UPlane() {
			hostBuf.UPlane()[i] = 128
			hostBuf.VPlane()[i] = 128
		}
	}

	outGIF := &gif.GIF{
		Image: make([]*image.Paletted, nFrames),
		Delay: make([]int, nFrames),
	}

	for i := 0; i < nFrames; i++ {
		var curHost *codec.YUVFrame
		if hostBuf != nil {
			upscaleGray4x(hostGray[i%len(hostGray)], sw, sh, hostBuf)
			curHost = hostBuf
		}
		// Perturb payload slightly each frame so data texture shimmers authentically like a real container!
		payload[0] = byte(i * 37)
		payload[len(payload)/2] = byte(i * 73)
		hdr := codec.FrameHeader{
			ContainerID: seed,
			ArtTheme:    themeID,
			Flags:       baseFlags,
			FrameIndex:  uint32(i * 2),
			TotalFrames: uint32(nFrames * 2),
		}
		if err := codec.EncodeFrameWithHost(dst, curHost, prof, hdr, payload); err != nil {
			return nil, err
		}
		zoom2x := !large && codec.IsGeometricArtMode(prof.ArtMode) && !useHost
		outGIF.Image[i] = downsampleYUVToPalettedSize(dst, pal, pw, ph, zoom2x)
		outGIF.Delay[i] = 14 // ~7.1 FPS smooth loop
	}

	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, outGIF); err != nil {
		return nil, err
	}
	raw := buf.Bytes()

	previewCacheMu.Lock()
	if len(previewCache) > 64 {
		previewCache = map[string][]byte{}
	}
	previewCache[cacheKey] = raw
	previewCacheMu.Unlock()

	return raw, nil
}

func (s *Server) handlePreviewGIF(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prof := q.Get("profile")
	theme := q.Get("theme")
	intensity := q.Get("intensity")
	hideHdr := q.Get("hide_header") == "1" || q.Get("hide_header") == "true"
	rawUseHost := q.Get("use_host")
	explicitHost := rawUseHost != ""
	useHost := rawUseHost == "1" || rawUseHost == "true"
	large := q.Get("size") == "large" || q.Get("size") == "hd"

	data, err := GeneratePreviewGIFCustom(prof, theme, intensity, hideHdr, useHost, explicitHost, large)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

func (s *Server) handlePreviewAudio(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	style := q.Get("style")
	mood := q.Get("mood")
	prompt := q.Get("prompt")
	var insts []string
	if raw := strings.TrimSpace(q.Get("instruments")); raw != "" {
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				insts = append(insts, p)
			}
		}
	}
	var seed uint64 = 0x9E3779B97F4A7C15
	if sStr := strings.TrimSpace(q.Get("seed")); sStr != "" {
		if parsed, err := strconv.ParseUint(sStr, 10, 64); err == nil && parsed != 0 {
			seed = parsed
		}
	}
	wav := codec.SynthMusicWAV(16.0, codec.MusicOptions{
		Style:       style,
		Mood:        mood,
		Instruments: insts,
		Prompt:      prompt,
		Seed:        seed,
	})
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(wav)
}

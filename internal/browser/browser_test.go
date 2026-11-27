package browser

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ytcloud/internal/media"
)

func TestYTBrowserDemoSiteEndToEndOpenAndEncrypted(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("FFmpeg não disponível")
	}

	srv, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()

	// 1. Open Optical Web (YTWB001 — zero passwords!) over Julia fractal video
	openMP4 := filepath.Join(tmpDir, "demo_open_julia.mp4")
	statsOpen, err := srv.PublishSiteToVideo(PublishOptions{
		UseDemoSite:   true,
		OutputPath:    openMP4,
		Resolution:    "720p",
		Profile:       "art-fullres",
		ArtTheme:      "julia-turing",
		FPS:           15,
		HideHeaderBar: true,
		TargetSeconds: 2,
	}, nil)
	if err != nil {
		t.Fatalf("publish open site failed: %v", err)
	}
	t.Logf("Open Demo Site MP4: %d frames, %.1f KB", statsOpen.TotalFrames, float64(statsOpen.VideoSizeBytes)/1024)

	siteOpen, needLayers, err := srv.MountVideoSite(openMP4, openMP4, "", openMP4, nil, nil)
	if err != nil || needLayers != 0 || siteOpen == nil {
		t.Fatalf("mount open site failed: site=%v needLayers=%d err=%v", siteOpen, needLayers, err)
	}
	if siteOpen.EncryptionMode != "open" || siteOpen.EntryPath != "index.html" {
		t.Fatalf("unexpected site metadata: mode=%s entry=%s", siteOpen.EncryptionMode, siteOpen.EntryPath)
	}
	if !strings.Contains(siteOpen.Title, "Portal Óptico Demo") {
		t.Fatalf("unexpected title: %q", siteOpen.Title)
	}

	// Verify HTTP serving + CSP isolation + ytweb:// bridge injection + CSS/JS/subpage
	h := srv.Handler()
	for _, tc := range []struct {
		path   string
		wantCT string
		substr string
	}{
		{"/site/" + siteOpen.ID + "/index.html", "text/html", "btweb-navigate"},
		{"/site/" + siteOpen.ID + "/security.html", "text/html", "Pesquisa White-Hat"},
		{"/site/" + siteOpen.ID + "/style.css", "text/css", "--accent:"},
		{"/site/" + siteOpen.ID + "/app.js", "application/javascript", "demo-canvas"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET %s -> %d", tc.path, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), tc.wantCT) {
			t.Fatalf("GET %s CT=%q want %q", tc.path, rec.Header().Get("Content-Type"), tc.wantCT)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("GET %s missing strict isolation CSP", tc.path)
		}
		if !strings.Contains(rec.Body.String(), tc.substr) {
			t.Fatalf("GET %s body missing %q", tc.path, tc.substr)
		}
	}

	// Verify sandboxed /site/* cannot access protected /api/* without token
	recAPI := httptest.NewRecorder()
	h.ServeHTTP(recAPI, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if recAPI.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/status returned %d, want 401", recAPI.Code)
	}

	// 2. Encrypted 2-Layer Post-Quantum Site (YTPUB001)
	encMP4 := filepath.Join(tmpDir, "demo_pq2_mandelbrot.mp4")
	pws := []string{"senha-alfa-2026", "segunda-camada-pq"}
	if _, err := srv.PublishSiteToVideo(PublishOptions{
		UseDemoSite:   true,
		OutputPath:    encMP4,
		Resolution:    "720p",
		Profile:       "art-fullres",
		ArtTheme:      "mandelbrot",
		FPS:           15,
		HideHeaderBar: true,
		Passwords:     pws,
	}, nil); err != nil {
		t.Fatalf("publish encrypted site failed: %v", err)
	}

	// Opening without password should cleanly report needLayers=2 (not crash!)
	siteLocked, layers, err := srv.MountVideoSite(encMP4, encMP4, "", encMP4, nil, nil)
	if err != nil || siteLocked != nil || layers != 2 {
		t.Fatalf("expected needLayers=2 without password, got site=%v layers=%d err=%v", siteLocked, layers, err)
	}

	// Opening with correct 2 passwords should succeed bit-exact!
	siteUnlocked, layers2, err := srv.MountVideoSite(encMP4, encMP4, "", encMP4, pws, nil)
	if err != nil || layers2 != 0 || siteUnlocked == nil || siteUnlocked.PasswordLayers != 2 {
		t.Fatalf("mount encrypted site failed: site=%v layers=%d err=%v", siteUnlocked, layers2, err)
	}
}

func TestYTBrowserMultiShardParallel4Videos(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("FFmpeg não disponível")
	}
	srv, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	baseMP4 := filepath.Join(tmpDir, "demo_sharded.mp4")

	// Publish Demo Site split across 4 parallel videos (YTSH001) using Stealth Stars!
	pubRes, err := srv.PublishSiteToVideo(PublishOptions{
		UseDemoSite:   true,
		OutputPath:    baseMP4,
		Resolution:    "720p",
		Profile:       "stealth-stars",
		ArtTheme:      "none",
		FPS:           15,
		HideHeaderBar: true,
		Shards:        4,
	}, nil)
	if err != nil {
		t.Fatalf("publish 4-shard site failed: %v", err)
	}
	if pubRes.ShardCount != 4 || len(pubRes.ShardPaths) != 4 {
		t.Fatalf("expected 4 shards, got count=%d paths=%d", pubRes.ShardCount, len(pubRes.ShardPaths))
	}
	t.Logf("4-Shard Stealth-Stars Site: %d total frames, %.1f KB across 4 videos", pubRes.TotalFrames, float64(pubRes.VideoSizeBytes)/1024)

	// 1. Mount explicitly with all 4 shard paths in parallel (even out-of-order!)
	reversed := []string{pubRes.ShardPaths[3], pubRes.ShardPaths[1], pubRes.ShardPaths[0], pubRes.ShardPaths[2]}
	siteAll, layers, err := srv.MountMultiVideoSite(reversed, pubRes.CombinedAddress, "", pubRes.CombinedAddress, nil, nil)
	if err != nil || layers != 0 || siteAll == nil {
		t.Fatalf("MountMultiVideoSite(4 shards) failed: site=%v layers=%d err=%v", siteAll, layers, err)
	}
	if siteAll.ShardCount != 4 || siteAll.EntryPath != "index.html" {
		t.Fatalf("unexpected sharded site meta: shards=%d entry=%s", siteAll.ShardCount, siteAll.EntryPath)
	}

	// 2. Auto-sibling discovery: open ONLY part 1 locally -> auto-finds parts 2, 3, 4 and mounts in parallel!
	siteAuto, layers2, err := srv.MountVideoSite(pubRes.ShardPaths[0], pubRes.ShardPaths[0], "", pubRes.ShardPaths[0], nil, nil)
	if err != nil || layers2 != 0 || siteAuto == nil || siteAuto.ShardCount != 4 {
		t.Fatalf("auto-sibling shard mount failed: site=%v layers=%d err=%v", siteAuto, layers2, err)
	}

	// 3. Verify ParseMultiAddress handles btweb:// (and legacy ytweb://) multi-shard addresses
	srcs, sp, _ := ParseMultiAddress("btweb://dQw4w9WgXcQ+aaaaaaaaaaa+bbbbbbbbbbb+ccccccccccc/security.html")
	if len(srcs) != 4 || sp != "security.html" {
		t.Fatalf("ParseMultiAddress btweb failed: srcs=%v sp=%q", srcs, sp)
	}
	srcsLegacy, spLegacy, _ := ParseMultiAddress("ytweb://dQw4w9WgXcQ+aaaaaaaaaaa/security.html")
	if len(srcsLegacy) != 2 || spLegacy != "security.html" {
		t.Fatalf("ParseMultiAddress legacy ytweb failed: srcs=%v sp=%q", srcsLegacy, spLegacy)
	}
}

func TestProgressiveLayerSiteStreamingAndHTTPOnline(t *testing.T) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		t.Skip("FFmpeg não disponível")
	}

	srv, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()

	// Build a multi-layer site: Camada 0 (index.html + style.css), Camada 1 (pages/about.html),
	// Camada 2 (assets/media/big.bin — 240 KB incompressible random bytes spanning many frames!)
	siteDir := filepath.Join(tmpDir, "layered_site")
	if err := os.MkdirAll(filepath.Join(siteDir, "pages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(siteDir, "assets", "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	indexHTML := []byte("<!doctype html><html><head><title>Site Progressivo Ao Vivo</title><link rel=\"stylesheet\" href=\"style.css\"></head><body><h1>Camada 0 Instantânea</h1><img src=\"assets/media/big.bin\"></body></html>")
	styleCSS := []byte("body { background: #0f172a; color: #f8fafc; font-family: sans-serif; }")
	aboutHTML := []byte("<!doctype html><html><head><title>Sobre</title></head><body><p>Camada 1</p></body></html>")
	bigBin := make([]byte, 240*1024)
	if _, err := rand.Read(bigBin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "index.html"), indexHTML, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "style.css"), styleCSS, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "pages", "about.html"), aboutHTML, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "assets", "media", "big.bin"), bigBin, 0o644); err != nil {
		t.Fatal(err)
	}

	outMP4 := filepath.Join(tmpDir, "progressive_site.mp4")
	pubRes, err := srv.PublishSiteToVideo(PublishOptions{
		FolderPath:    siteDir,
		OutputPath:    outMP4,
		Resolution:    "720p",
		Profile:       "art-fullres",
		ArtTheme:      "julia-turing",
		FPS:           15,
		HideHeaderBar: true,
		CrossFECMode:  "max",
	}, nil)
	if err != nil {
		t.Fatalf("publish layered site failed: %v", err)
	}
	t.Logf("Layered site MP4: %d frames, %.1f KB", pubRes.TotalFrames, float64(pubRes.VideoSizeBytes)/1024)

	// Serve the MP4 over real HTTP to test zero-disk direct online streaming ("assistindo online")!
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, outMP4)
	}))
	defer fileSrv.Close()
	onlineURL := fileSrv.URL + "/progressive_site.mp4"

	site, shouldFallback, err := srv.tryMountProgressiveStream(onlineURL, "", onlineURL, "test-prog-key", nil)
	if err != nil || shouldFallback || site == nil {
		t.Fatalf("tryMountProgressiveStream(HTTP) failed: site=%v fallback=%v err=%v", site, shouldFallback, err)
	}
	snap0 := site.snapshotJSON()
	t.Logf("First Paint ready! mode=%s streaming=%v loaded=%d/%d title=%q", snap0.StreamMode, snap0.Streaming, snap0.LoadedFiles, snap0.TotalFiles, snap0.Title)
	if snap0.StreamMode != "online-live" {
		t.Fatalf("expected StreamMode=online-live, got %q", snap0.StreamMode)
	}
	if snap0.EntryPath != "index.html" || snap0.Title != "Site Progressivo Ao Vivo" {
		t.Fatalf("unexpected first-paint meta: entry=%q title=%q", snap0.EntryPath, snap0.Title)
	}

	h := srv.Handler()

	// 1. Camada 0 index.html serves immediately
	recIdx := httptest.NewRecorder()
	h.ServeHTTP(recIdx, httptest.NewRequest(http.MethodGet, "/site/"+site.ID+"/index.html", nil))
	if recIdx.Code != 200 || !strings.Contains(recIdx.Body.String(), "Camada 0 Instantânea") {
		t.Fatalf("GET index.html -> %d", recIdx.Code)
	}

	// 2. Non-existent path (/favicon.ico) returns 404 immediately without stalling
	t0 := time.Now()
	rec404 := httptest.NewRecorder()
	h.ServeHTTP(rec404, httptest.NewRequest(http.MethodGet, "/site/"+site.ID+"/favicon.ico", nil))
	if rec404.Code != 404 {
		t.Fatalf("GET missing favicon.ico -> %d, want 404", rec404.Code)
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("404 took too long (%v) — should never wait for non-manifest files", time.Since(t0))
	}

	// 3. Camada 2 big.bin waits cleanly (if still streaming) and delivers 100% bit-exact 240 KB!
	recBig := httptest.NewRecorder()
	h.ServeHTTP(recBig, httptest.NewRequest(http.MethodGet, "/site/"+site.ID+"/assets/media/big.bin", nil))
	if recBig.Code != 200 {
		t.Fatalf("GET assets/media/big.bin -> %d", recBig.Code)
	}
	if !bytes.Equal(recBig.Body.Bytes(), bigBin) {
		t.Fatalf("assets/media/big.bin bytes mismatch (got %d bytes, want %d)", recBig.Body.Len(), len(bigBin))
	}

	// 4. Camada 1 pages/about.html also serves cleanly
	recAbout := httptest.NewRecorder()
	h.ServeHTTP(recAbout, httptest.NewRequest(http.MethodGet, "/site/"+site.ID+"/pages/about.html", nil))
	if recAbout.Code != 200 || !strings.Contains(recAbout.Body.String(), "Camada 1") {
		t.Fatalf("GET pages/about.html -> %d", recAbout.Code)
	}

	// 5. Wait briefly for background streaming goroutine to reach Streaming == false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snap := site.snapshotJSON()
		if !snap.Streaming {
			if snap.StreamError != "" {
				t.Fatalf("unexpected stream error: %s", snap.StreamError)
			}
			if snap.LoadedFiles != 4 || snap.TotalFiles != 4 {
				t.Fatalf("expected 4/4 files loaded, got %d/%d", snap.LoadedFiles, snap.TotalFiles)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if site.snapshotJSON().Streaming {
		t.Fatalf("site remained in Streaming=true state after deadline")
	}

	// 6. Verify encrypted site (YTPUB001) triggers clean fallback instead of failing
	encMP4 := filepath.Join(tmpDir, "enc_fallback.mp4")
	if _, err := srv.PublishSiteToVideo(PublishOptions{
		UseDemoSite:   true,
		OutputPath:    encMP4,
		Resolution:    "720p",
		Profile:       "art-fullres",
		ArtTheme:      "mandelbrot",
		FPS:           15,
		HideHeaderBar: true,
		Passwords:     []string{"segredo-123"},
	}, nil); err != nil {
		t.Fatalf("publish encrypted fallback site failed: %v", err)
	}
	encSite, encFallback, encErr := srv.tryMountProgressiveStream(encMP4, "", encMP4, "enc-key", nil)
	if encSite != nil || !encFallback || encErr != nil {
		t.Fatalf("encrypted video should cleanly request fallback: site=%v fallback=%v err=%v", encSite, encFallback, encErr)
	}

	// 7. BrightNet/3 CAS SHA-256 Instant Cache: isolate CAS dir, mount once (populates CAS), mount again -> cas-instant!
	srv.cas = NewContentCache(tmpDir, 64*1024*1024)
	siteFirst, fb1, err1 := srv.tryMountProgressiveStream(outMP4, "", outMP4, "cas-k1", nil)
	if err1 != nil || fb1 || siteFirst == nil {
		t.Fatalf("first CAS mount failed: %v", err1)
	}
	// Wait briefly for background stream + async CAS puts to complete
	for i := 0; i < 60; i++ {
		if !siteFirst.snapshotJSON().Streaming {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	siteSecond, fb2, err2 := srv.tryMountProgressiveStream(outMP4, "", outMP4, "cas-k2", nil)
	if err2 != nil || fb2 || siteSecond == nil {
		t.Fatalf("second CAS mount failed: %v", err2)
	}
	snapCAS := siteSecond.snapshotJSON()
	t.Logf("BrightNet/3 CAS Re-mount: mode=%s hits=%d/%d streaming=%v", snapCAS.StreamMode, snapCAS.CASCacheHits, snapCAS.TotalFiles, snapCAS.Streaming)
	if snapCAS.StreamMode != "cas-instant" || snapCAS.CASCacheHits != snapCAS.TotalFiles {
		t.Fatalf("expected cas-instant with %d/%d hits, got mode=%q hits=%d/%d", snapCAS.TotalFiles, snapCAS.TotalFiles, snapCAS.StreamMode, snapCAS.CASCacheHits, snapCAS.TotalFiles)
	}

	// 8. Real Cancellation of PublishSiteToVideo via context.CancelFunc (never hangs!)
	cancelCtx, cancelFn := context.WithCancel(context.Background())
	cancelMP4 := filepath.Join(tmpDir, "cancelled.mp4")
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancelFn()
	}()
	_, cancelErr := srv.PublishSiteToVideo(PublishOptions{
		FolderPath:    siteDir,
		OutputPath:    cancelMP4,
		Resolution:    "720p",
		Profile:       "art-fullres",
		ArtTheme:      "julia-turing",
		FPS:           15,
		TargetSeconds: 20, // long enough that cancel fires mid-encode
		Ctx:           cancelCtx,
	}, nil)
	if !errors.Is(cancelErr, context.Canceled) {
		t.Fatalf("expected context.Canceled from cancelled publish, got: %v", cancelErr)
	}
}

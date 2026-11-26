package browser

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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

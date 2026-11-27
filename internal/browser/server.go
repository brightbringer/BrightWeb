package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ytcloud/internal/codec"
	"ytcloud/internal/crypto"
	"ytcloud/internal/media"
	"ytcloud/internal/packer"
	"ytcloud/internal/server"
	"ytcloud/internal/sysutil"
	"ytcloud/internal/updater"
	"ytcloud/internal/youtube"
)

//go:embed web/*
var webFS embed.FS

// MountedSite holds a decoded optical website strictly in RAM, ready to be served via /site/<ID>/*.
// Supports progressive layer-by-layer streaming (Camada 0 opens live while Camadas 1..N stream in background).
type MountedSite struct {
	ID             string            `json:"site_id"`
	Source         string            `json:"source"`
	DisplayAddress string            `json:"display_address"`
	EntryPath      string            `json:"entry_path"`
	Title          string            `json:"title"`
	EncryptionMode string            `json:"encryption_mode"` // "open" (YTWB001) or "public-pq" (YTPUB001)
	PasswordLayers int               `json:"password_layers"`
	Resolution     string            `json:"resolution"`
	ProfileCode    string            `json:"profile_code"`
	ShardCount     int               `json:"shard_count,omitempty"`
	TotalFrames    int               `json:"total_frames"`
	DecodedFrames  int               `json:"decoded_frames,omitempty"`
	CorrectedBytes int               `json:"corrected_bytes"`
	TotalBytes     int64             `json:"total_bytes"`
	Streaming      bool              `json:"streaming,omitempty"`
	StreamMode     string            `json:"stream_mode,omitempty"` // "online-live", "local-live", "cas-instant", ""
	LoadedFiles    int               `json:"loaded_files,omitempty"`
	TotalFiles     int               `json:"total_files,omitempty"`
	CASCacheHits   int               `json:"cas_cache_hits,omitempty"`
	StreamError    string            `json:"stream_error,omitempty"`
	FileList       []SiteFileSummary `json:"files"`
	DecodedAt      string            `json:"decoded_at"`
	VideoLocalPath string            `json:"video_local_path,omitempty"`
	RootPrefix     string            `json:"-"`
	Files          map[string][]byte `json:"-"`

	mu            sync.RWMutex    `json:"-"`
	cond          *sync.Cond      `json:"-"`
	expectedFiles map[string]bool `json:"-"`
}

// snapshotJSON returns a lock-free copy of MountedSite metadata safe for JSON encoding while streaming.
func (m *MountedSite) snapshotJSON() MountedSite {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := MountedSite{
		ID:             m.ID,
		Source:         m.Source,
		DisplayAddress: m.DisplayAddress,
		EntryPath:      m.EntryPath,
		Title:          m.Title,
		EncryptionMode: m.EncryptionMode,
		PasswordLayers: m.PasswordLayers,
		Resolution:     m.Resolution,
		ProfileCode:    m.ProfileCode,
		ShardCount:     m.ShardCount,
		TotalFrames:    m.TotalFrames,
		DecodedFrames:  m.DecodedFrames,
		CorrectedBytes: m.CorrectedBytes,
		TotalBytes:     m.TotalBytes,
		Streaming:      m.Streaming,
		StreamMode:     m.StreamMode,
		LoadedFiles:    m.LoadedFiles,
		TotalFiles:     m.TotalFiles,
		CASCacheHits:   m.CASCacheHits,
		StreamError:    m.StreamError,
		DecodedAt:      m.DecodedAt,
		VideoLocalPath: m.VideoLocalPath,
	}
	if len(m.FileList) > 0 {
		cp.FileList = make([]SiteFileSummary, len(m.FileList))
		copy(cp.FileList, m.FileList)
	}
	return cp
}

func (m *MountedSite) lookupFileLocked(reqPath string) (string, []byte, bool) {
	if data, ok := m.Files[reqPath]; ok {
		return reqPath, data, true
	}
	for k, v := range m.Files {
		if strings.EqualFold(k, reqPath) {
			return k, v, true
		}
	}
	return "", nil, false
}

func (m *MountedSite) isExpectedLocked(reqPath string) bool {
	if len(m.expectedFiles) == 0 {
		return false
	}
	if m.expectedFiles[reqPath] {
		return true
	}
	for k := range m.expectedFiles {
		if strings.EqualFold(k, reqPath) {
			return true
		}
	}
	return false
}

// waitForFile returns the requested file immediately if loaded, or waits cleanly while the site is
// streaming if the file is listed in the manifest (returning instant 404 for non-existent paths like /favicon.ico).
func (m *MountedSite) waitForFile(ctx context.Context, reqPath string) (string, []byte, bool) {
	m.mu.RLock()
	if p, data, ok := m.lookupFileLocked(reqPath); ok {
		m.mu.RUnlock()
		return p, data, true
	}
	if !m.Streaming || m.cond == nil || !m.isExpectedLocked(reqPath) {
		m.mu.RUnlock()
		return "", nil, false
	}
	m.mu.RUnlock()

	timedOut := atomic.Bool{}
	timer := time.AfterFunc(90*time.Second, func() {
		timedOut.Store(true)
		m.mu.Lock()
		m.cond.Broadcast()
		m.mu.Unlock()
	})
	defer timer.Stop()

	stopCtxWake := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		m.cond.Broadcast()
		m.mu.Unlock()
	})
	defer stopCtxWake()

	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		if p, data, ok := m.lookupFileLocked(reqPath); ok {
			return p, data, true
		}
		if !m.Streaming || timedOut.Load() || ctx.Err() != nil {
			return "", nil, false
		}
		m.cond.Wait()
	}
}

type SiteFileSummary struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
	SHA256   string `json:"sha256"`
}

type BrowserJob struct {
	ID           string             `json:"id"`
	Kind         string             `json:"kind"` // "navigate", "publish"
	Status       string             `json:"status"` // "running", "done", "needs_password", "cancelled", "error"
	Progress     int                `json:"progress"`
	Detail       string             `json:"detail"`
	Speed        string             `json:"speed,omitempty"`
	ETA          string             `json:"eta,omitempty"`
	Error        string             `json:"error,omitempty"`
	Result       any                `json:"result,omitempty"`
	NeedLayers   int                `json:"password_layers,omitempty"`
	CachedVideo  string             `json:"cached_video,omitempty"`
	StartedAt    string             `json:"started_at"`
	EndedAt      string             `json:"ended_at,omitempty"`
	cancel       context.CancelFunc `json:"-"`
}

type Server struct {
	token            string
	cacheDir         string
	cas              *ContentCache
	mux              *http.ServeMux
	shutdownCh       chan struct{}
	shutdownOnce     sync.Once
	lastHeartbeat    atomic.Int64
	clientConnected  atomic.Bool
	activeSSEClients atomic.Int32
	hadSSEClient     atomic.Bool
	activeDialogs    atomic.Int32
	initialTarget    string

	mu           sync.RWMutex
	sites        map[string]*MountedSite // siteID -> MountedSite
	siteByKey    map[string]string       // cacheKey -> siteID
	jobs         map[string]*BrowserJob
	prefs        BrowserPrefs
	dnsBook      DNSBook
	dnsFilePath  string
	updater      *updater.Manager
	isolatedMode atomic.Bool
}

func New(initialTarget string) (*Server, error) {
	tok, err := crypto.RandomHex(24)
	if err != nil {
		return nil, err
	}
	cacheDir := filepath.Join(media.DefaultAppDir(), "brightweb_cache")
	_ = os.MkdirAll(cacheDir, 0o755)

	s := &Server{
		token:         tok,
		cacheDir:      cacheDir,
		cas:           NewContentCache(cacheDir, 512*1024*1024),
		mux:           http.NewServeMux(),
		shutdownCh:    make(chan struct{}),
		initialTarget: strings.TrimSpace(initialTarget),
		sites:         make(map[string]*MountedSite),
		siteByKey:     make(map[string]string),
		jobs:          make(map[string]*BrowserJob),
		updater:       updater.NewManager("BrightWeb", "brightbringer", "BrightWeb", "BrightWeb.exe", media.DefaultAppDir()),
	}
	s.prefs = s.loadPrefsLocked()
	s.dnsBook = s.loadDNSBookLocked()
	s.isolatedMode.Store(true) // White-hat strict offline CSP ON by default!
	s.registerRoutes()
	s.updater.StartScheduler()
	return s, nil
}

func (s *Server) ShutdownChan() <-chan struct{} { return s.shutdownCh }
func (s *Server) Token() string                 { return s.token }
func (s *Server) Handler() http.Handler         { return s.authMiddleware(s.mux) }

func (s *Server) cancelAllJobs() {
	s.mu.Lock()
	var cancels []context.CancelFunc
	for _, j := range s.jobs {
		if j.Status == "running" && j.cancel != nil {
			cancels = append(cancels, j.cancel)
		}
	}
	s.mu.Unlock()
	for _, fn := range cancels {
		fn()
	}
}

func (s *Server) TriggerShutdown() {
	s.cancelAllJobs()
	s.shutdownOnce.Do(func() {
		close(s.shutdownCh)
	})
}

func (s *Server) Start(addr string) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	appURL := fmt.Sprintf("http://%s/?token=%s", ln.Addr().String(), s.token)
	go func() {
		srv := &http.Server{Handler: s.Handler()}
		_ = srv.Serve(ln)
	}()
	// Auto-shutdown watchdog once desktop browser window closes
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.shutdownCh:
				return
			case <-ticker.C:
				if !s.clientConnected.Load() {
					continue
				}
				// Never shut down while any window holds an open SSE stream (immune to Edge/Chrome background timer throttling!)
				if s.activeSSEClients.Load() > 0 {
					s.lastHeartbeat.Store(time.Now().Unix())
					continue
				}
				grace := 5 * time.Minute
				if s.hadSSEClient.Load() {
					grace = 20 * time.Second
				}
				last := s.lastHeartbeat.Load()
				if last > 0 && time.Since(time.Unix(last, 0)) > grace && !s.hasRunningJobs() && s.activeDialogs.Load() == 0 {
					s.TriggerShutdown()
					return
				}
			}
		}
	}()
	return ln, appURL, nil
}

// LaunchBrowserWindow opens BrightWeb as a clean maximized standalone desktop browser window.
func LaunchBrowserWindow(appURL string) {
	if runtime.GOOS == "windows" {
		candidates := []string{
			filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "Application", "chrome.exe"),
		}
		for _, bin := range candidates {
			if _, err := os.Stat(bin); err == nil {
				cmd := exec.Command(bin, "--app="+appURL, "--start-maximized", "--new-window")
				if cmd.Start() == nil {
					go sysutil.MaximizeWindowByTitle("BrightWeb —", 6*time.Second)
					return
				}
			}
		}
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", appURL).Start()
		go sysutil.MaximizeWindowByTitle("BrightWeb —", 6*time.Second)
		return
	}
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", appURL).Start()
	} else {
		_ = exec.Command("xdg-open", appURL).Start()
	}
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if qTok := r.URL.Query().Get("token"); qTok != "" && qTok == s.token {
			http.SetCookie(w, &http.Cookie{
				Name:     "ytbrowser_session",
				Value:    s.token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			if r.URL.Path == "/" {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
		}
		// Strictly protect /api/* — opaque sandboxed iframes (/site/*) NEVER get this cookie or token!
		if strings.HasPrefix(r.URL.Path, "/api/") {
			ok := r.Header.Get("X-YTBrowser-Token") == s.token
			if !ok {
				if c, err := r.Cookie("ytbrowser_session"); err == nil && c.Value == s.token {
					ok = true
				}
			}
			if !ok {
				writeError(w, http.StatusUnauthorized, "sessão não autorizada")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerRoutes() {
	sub, _ := fs.Sub(webFS, "web")
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
	s.mux.HandleFunc("/site/", s.handleServeSite)

	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/prefs", s.handlePrefs)
	s.mux.HandleFunc("/api/update/status", s.handleUpdateStatus)
	s.mux.HandleFunc("/api/update/check", s.handleUpdateCheck)
	s.mux.HandleFunc("/api/update/apply", s.handleUpdateApply)
	s.mux.HandleFunc("/api/heartbeat", s.handleHeartbeat)
	s.mux.HandleFunc("/api/events", s.handleEvents)
	s.mux.HandleFunc("/api/shutdown", s.handleShutdown)
	s.mux.HandleFunc("/api/settings/isolated", s.handleToggleIsolated)
	s.mux.HandleFunc("/api/navigate", s.handleNavigate)
	s.mux.HandleFunc("/api/jobs/", s.handleJobStatus)
	s.mux.HandleFunc("/api/sites/", s.handleSiteMeta)
	s.mux.HandleFunc("/api/publish", s.handlePublish)
	s.mux.HandleFunc("/api/publish/demo", s.handlePublishDemo)
	s.mux.HandleFunc("/api/preview/gif", s.handlePreviewGIF)
	s.mux.HandleFunc("/api/preview/audio", s.handlePreviewAudio)
	s.mux.HandleFunc("/api/media/probe-duration", s.handleProbeMediaDuration)
	s.mux.HandleFunc("/api/system/path-stats", s.handlePathStats)
	s.mux.HandleFunc("/api/system/pick-video", s.handlePickVideo)
	s.mux.HandleFunc("/api/system/pick-audio", s.handlePickAudio)
	s.mux.HandleFunc("/api/system/pick-folder", s.handlePickFolder)
	s.mux.HandleFunc("/api/system/open-folder", s.handleOpenFolder)
	s.mux.HandleFunc("/api/system/open-external", s.handleOpenExternal)
	s.mux.HandleFunc("/api/dns", s.handleDNSGet)
	s.mux.HandleFunc("/api/dns/save", s.handleDNSSave)
	s.mux.HandleFunc("/api/dns/delete", s.handleDNSDelete)
	s.mux.HandleFunc("/api/dns/reorder", s.handleDNSReorder)
	s.mux.HandleFunc("/api/dns/favorite", s.handleDNSToggleFavorite)
	s.mux.HandleFunc("/api/dns/export", s.handleDNSExport)
	s.mux.HandleFunc("/api/dns/import", s.handleDNSImport)
}

func (s *Server) hasRunningJobs() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.jobs {
		if j.Status == "running" {
			return true
		}
	}
	return false
}

func (s *Server) createJob(kind, detail string) *BrowserJob {
	id, _ := crypto.RandomHex(8)
	j := &BrowserJob{
		ID:        id,
		Kind:      kind,
		Status:    "running",
		Progress:  2,
		Detail:    detail,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()
	return j
}

func (s *Server) updateJob(j *BrowserJob, pct int, detail, speed, eta string) {
	if j == nil {
		return
	}
	s.mu.Lock()
	if pct > j.Progress {
		j.Progress = pct
	}
	if detail != "" {
		j.Detail = detail
	}
	j.Speed = speed
	j.ETA = eta
	s.mu.Unlock()
}

func (s *Server) finishJob(j *BrowserJob, status string, res any, err error) {
	s.mu.Lock()
	j.Status = status
	j.EndedAt = time.Now().Format(time.RFC3339)
	if err != nil {
		j.Error = err.Error()
	} else {
		j.Progress = 100
		j.Result = res
	}
	s.mu.Unlock()
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	tools := media.DetectTools()
	s.mu.RLock()
	prefs := s.prefs
	dnsBook := s.dnsBook
	s.mu.RUnlock()
	resList := []codec.Resolution{
		codec.Resolutions["720p"],
		codec.Resolutions["1080p"],
		codec.Resolutions["2k"],
		codec.Resolutions["4k"],
	}
	profList := codec.ListProfiles()
	frameCaps := make(map[string]map[string]int, len(resList))
	frameCapsDetailed := make(map[string]map[string]map[string]int, len(resList))
	for _, rObj := range resList {
		m := make(map[string]int, len(profList))
		md := make(map[string]map[string]int, len(profList))
		for _, pObj := range profList {
			m[pObj.Code] = pObj.PayloadCapacity(rObj.Width, rObj.Height)
			if pObj.ArtMode != "" {
				im := make(map[string]int, 8)
				for _, mi := range []uint8{codec.MaskIntensityDefault, codec.MaskIntensitySubtle, codec.MaskIntensityStrong, codec.MaskIntensityMax} {
					for _, hh := range []bool{false, true} {
						hhFlag := hh && codec.CanHideHeaderArtMode(pObj.ArtMode, false)
						fl := codec.BuildHeaderFlags(mi, hhFlag)
						k := fmt.Sprintf("%d_%d", mi, map[bool]int{false: 0, true: 1}[hh])
						im[k] = pObj.PayloadCapacityLayoutWithFlags(rObj.Width, rObj.Height, true, fl)
						if pObj.ArtMode == "subtractive-y2" || pObj.ArtMode == "video-bw" {
							hostFl := codec.BuildHeaderFlags(mi, hh) | codec.FlagHostManchester
							im[k+"_host"] = pObj.PayloadCapacityLayoutWithFlags(rObj.Width, rObj.Height, true, hostFl)
						}
					}
				}
				md[pObj.Code] = im
			}
		}
		frameCaps[rObj.Code] = m
		frameCapsDetailed[rObj.Code] = md
	}
	writeJSON(w, map[string]any{
		"version":                   updater.AppVersion,
		"update":                    s.updater.Status(),
		"prefs":                     prefs,
		"dns_book":                  dnsBook,
		"tools":                     tools,
		"isolated_mode":             s.isolatedMode.Load(),
		"initial_target":            s.initialTarget,
		"art_themes":                codec.ListArtThemes(),
		"profiles":                  profList,
		"resolutions":               resList,
		"frame_capacities":          frameCaps,
		"frame_capacities_detailed": frameCapsDetailed,
		"cache_dir":                 s.cacheDir,
	})
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

	data, err := server.GeneratePreviewGIFCustom(prof, theme, intensity, hideHdr, useHost, explicitHost, large)
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

func (s *Server) handlePathStats(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	p := strings.Trim(strings.TrimSpace(req.Path), `"`)
	if p == "" {
		writeJSON(w, map[string]any{"file_count": 0, "total_bytes": 0, "has_index_html": false})
		return
	}
	inputs, err := packer.CollectInputsFromPath(p)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	var totalBytes int64
	hasIndex := false
	for _, inp := range inputs {
		if fi, err := os.Stat(inp.LocalPath); err == nil {
			totalBytes += fi.Size()
		}
		base := strings.ToLower(filepath.Base(inp.RelPath))
		if base == "index.html" || base == "index.htm" {
			hasIndex = true
		}
	}
	writeJSON(w, map[string]any{
		"file_count":     len(inputs),
		"total_bytes":    totalBytes,
		"has_index_html": hasIndex,
	})
}

func (s *Server) handleProbeMediaDuration(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	src := strings.Trim(strings.TrimSpace(req.Source), `"`)
	if src == "" {
		writeError(w, 400, "informe um arquivo local ou link")
		return
	}
	tools := media.DetectTools()
	lower := strings.ToLower(src)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		if !tools.HasYtDlp {
			writeError(w, 400, "yt-dlp necessário para consultar duração de links")
			return
		}
		args := []string{"--no-playlist", "--no-warnings", "--print", "%(duration)s|%(title)s"}
		if tools.JSRuntime != "" {
			args = append(args, "--js-runtimes", tools.JSRuntime)
		}
		args = append(args, src)
		cmd := exec.Command(tools.YtDlpPath, args...)
		sysutil.ApplySubprocessAttrs(cmd, true)
		out, err := cmd.Output()
		if err != nil {
			writeError(w, 400, "não foi possível consultar a duração do link")
			return
		}
		line := strings.TrimSpace(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
		parts := strings.SplitN(line, "|", 2)
		dur, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		if err != nil || dur <= 0 {
			writeError(w, 400, "duração não disponível para este link")
			return
		}
		title := ""
		if len(parts) > 1 {
			title = strings.TrimSpace(parts[1])
		}
		writeJSON(w, map[string]any{"duration_sec": dur, "title": title})
		return
	}
	if _, err := os.Stat(src); err != nil {
		writeError(w, 400, "arquivo de mídia não encontrado")
		return
	}
	dur, err := codec.ProbeMediaDuration(tools.FFprobePath, tools.FFmpegPath, src)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"duration_sec": dur, "title": filepath.Base(src)})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	s.clientConnected.Store(true)
	s.lastHeartbeat.Store(time.Now().Unix())
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	s.clientConnected.Store(true)
	s.hadSSEClient.Store(true)
	s.activeSSEClients.Add(1)
	s.lastHeartbeat.Store(time.Now().Unix())
	defer func() {
		s.activeSSEClients.Add(-1)
		s.lastHeartbeat.Store(time.Now().Unix())
	}()
	_, _ = w.Write([]byte(": connected\n\n"))
	flusher.Flush()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			s.lastHeartbeat.Store(time.Now().Unix())
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
	go func() {
		time.Sleep(150 * time.Millisecond)
		s.TriggerShutdown()
	}()
}

func (s *Server) handleToggleIsolated(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Isolated bool `json:"isolated"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	s.isolatedMode.Store(req.Isolated)
	writeJSON(w, map[string]any{"isolated_mode": req.Isolated})
}

func (s *Server) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	if strings.HasSuffix(trimmed, "/cancel") && r.Method == http.MethodPost {
		id := strings.TrimSuffix(trimmed, "/cancel")
		s.mu.Lock()
		j, ok := s.jobs[id]
		var cancelFn context.CancelFunc
		if ok && j.Status == "running" {
			cancelFn = j.cancel
			j.Detail = "Cancelando geração (encerrando FFmpeg)..."
		}
		s.mu.Unlock()
		if !ok {
			writeError(w, 404, "tarefa não encontrada")
			return
		}
		if cancelFn != nil {
			cancelFn()
		}
		writeJSON(w, map[string]any{"ok": true, "id": id})
		return
	}
	s.mu.RLock()
	j, ok := s.jobs[trimmed]
	s.mu.RUnlock()
	if !ok {
		writeError(w, 404, "tarefa não encontrada")
		return
	}
	writeJSON(w, j)
}

func (s *Server) handleSiteMeta(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sites/")
	s.mu.RLock()
	site, ok := s.sites[id]
	s.mu.RUnlock()
	if !ok {
		writeError(w, 404, "site não encontrado na memória")
		return
	}
	snap := site.snapshotJSON()
	writeJSON(w, &snap)
}

// stripOpticalScheme removes btweb:// (primary) or legacy ytweb:// prefix if present.
func stripOpticalScheme(s string) (string, bool) {
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "btweb://") {
		return s[len("btweb://"):], true
	}
	if strings.HasPrefix(low, "ytweb://") {
		return s[len("ytweb://"):], true
	}
	return s, false
}

// ParseAddress normalizes btweb:// (and legacy ytweb://), YouTube URLs, direct URLs, or local paths into (videoSource, subPath, displayAddr).
func ParseAddress(raw string) (videoSource string, subPath string, displayAddr string) {
	s := strings.Trim(strings.TrimSpace(raw), `"`)
	if s == "" {
		return "", "", ""
	}
	// Handle btweb:// (and legacy ytweb://) protocol
	if rest, ok := stripOpticalScheme(s); ok {
		if strings.HasPrefix(strings.ToLower(rest), "http://") || strings.HasPrefix(strings.ToLower(rest), "https://") {
			return rest, "", s
		}
		parts := strings.SplitN(rest, "/", 2)
		idPart := strings.TrimSpace(parts[0])
		if len(parts) == 2 {
			subPath = strings.TrimPrefix(parts[1], "/")
		}
		if len(idPart) == 11 && youtube.ExtractVideoID(idPart) == idPart {
			return "https://www.youtube.com/watch?v=" + idPart, subPath, s
		}
		return idPart, subPath, s
	}
	// Plain 11-char YouTube ID
	if len(s) == 11 && youtube.ExtractVideoID(s) == s {
		return "https://www.youtube.com/watch?v=" + s, "", "btweb://" + s
	}
	return s, "", s
}

// ParseMultiAddress parses either a single address or a multi-video sharded address such as:
//   - btweb://ID1+ID2+ID3+ID4[/subpath] (also accepts legacy ytweb://)
//   - https://youtu.be/ID1 | https://youtu.be/ID2
//   - C:\path\site_part1of4.mp4 | C:\path\site_part2of4.mp4
func ParseMultiAddress(raw string) (sources []string, subPath string, displayAddr string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, "", ""
	}
	// 1. btweb://ID1+ID2+ID3[/subpath] (or legacy ytweb://)
	if rest, ok := stripOpticalScheme(s); ok {
		parts := strings.SplitN(rest, "/", 2)
		idBlob := strings.TrimSpace(parts[0])
		if len(parts) == 2 {
			subPath = strings.TrimPrefix(parts[1], "/")
		}
		if strings.Contains(idBlob, "+") {
			rawIDs := strings.Split(idBlob, "+")
			for _, id := range rawIDs {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				src, _, _ := ParseAddress("btweb://" + id)
				sources = append(sources, src)
			}
			return sources, subPath, s
		}
	}
	// 2. Explicit multi-source separators: " | ", ";", or newlines (safe with Windows paths & URLs!)
	sep := ""
	switch {
	case strings.Contains(s, "|"):
		sep = "|"
	case strings.Contains(s, ";"):
		sep = ";"
	case strings.Contains(s, "\n"):
		sep = "\n"
	}
	if sep != "" {
		for _, tok := range strings.Split(s, sep) {
			tok = strings.Trim(strings.TrimSpace(tok), `"`)
			if tok == "" {
				continue
			}
			src, sp, _ := ParseAddress(tok)
			if sp != "" && subPath == "" {
				subPath = sp
			}
			sources = append(sources, src)
		}
		return sources, subPath, s
	}
	src, sp, disp := ParseAddress(s)
	return []string{src}, sp, disp
}

// discoverLocalShardSiblings finds matching local _partXofN.<ext> siblings in the same folder
// when a user opens just 1 shard file (e.g. site_20261125_part1of4.mp4).
func discoverLocalShardSiblings(firstPath string, shardCount int) []string {
	dir := filepath.Dir(firstPath)
	ext := filepath.Ext(firstPath)
	base := strings.TrimSuffix(filepath.Base(firstPath), ext)
	suffix := fmt.Sprintf("of%d", shardCount)
	idx := strings.LastIndex(base, "_part")
	if idx < 0 || !strings.HasSuffix(base, suffix) {
		return nil
	}
	prefix := base[:idx]
	out := make([]string, 0, shardCount)
	for i := 1; i <= shardCount; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s_part%dof%d%s", prefix, i, shardCount, ext))
		if _, err := os.Stat(cand); err == nil {
			out = append(out, cand)
		}
	}
	if len(out) == shardCount {
		return out
	}
	return nil
}

func isRemoteURL(src string) bool {
	l := strings.ToLower(src)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

func (s *Server) findCachedVideo(src string) string {
	h := sha256.Sum256([]byte(src))
	key := hex.EncodeToString(h[:8])
	matches, _ := filepath.Glob(filepath.Join(s.cacheDir, "vid_"+key+".*"))
	for _, m := range matches {
		if !strings.HasSuffix(m, ".part") {
			if fi, err := os.Stat(m); err == nil && fi.Size() > 1024 {
				return m
			}
		}
	}
	return ""
}

func (s *Server) resolveOrDownloadVideo(src string, j *BrowserJob) (string, error) {
	if !isRemoteURL(src) {
		if _, err := os.Stat(src); err != nil {
			return "", fmt.Errorf("arquivo de vídeo não encontrado: %s", src)
		}
		return src, nil
	}
	// Check local video cache by URL hash so revisiting or unlocking with password is instant!
	if cached := s.findCachedVideo(src); cached != "" {
		s.updateJob(j, 42, "Vídeo encontrado no cache local — pulando download!", "", "")
		return cached, nil
	}
	h := sha256.Sum256([]byte(src))
	key := hex.EncodeToString(h[:8])

	// Direct HTTP video file download (.mp4 / .mkv / .webm) without needing yt-dlp
	lowerPath := strings.ToLower(strings.SplitN(src, "?", 2)[0])
	if strings.HasSuffix(lowerPath, ".mp4") || strings.HasSuffix(lowerPath, ".mkv") || strings.HasSuffix(lowerPath, ".webm") {
		s.updateJob(j, 10, "Baixando vídeo direto via HTTP...", "", "")
		ext := filepath.Ext(lowerPath)
		dst := filepath.Join(s.cacheDir, "vid_"+key+ext)
		resp, err := http.Get(src)
		if err != nil {
			return "", fmt.Errorf("erro baixando vídeo HTTP: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("servidor retornou HTTP %d", resp.StatusCode)
		}
		f, err := os.Create(dst)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(f, resp.Body)
		_ = f.Close()
		if err != nil {
			_ = os.Remove(dst)
			return "", err
		}
		return dst, nil
	}

	// Download from YouTube or any platform supported by yt-dlp (Vimeo, Archive.org, Dailymotion, etc.)
	tools := media.DetectTools()
	if !tools.HasYtDlp {
		return "", errors.New("yt-dlp não encontrado — abra o BrightVault uma vez para instalar FFmpeg/yt-dlp ou instale-os na pasta bin")
	}
	s.updateJob(j, 10, "Baixando vídeo na qualidade máxima via yt-dlp...", "", "")
	dlOpts := youtube.YtDlpOptions{
		YtDlpPath:  tools.YtDlpPath,
		FFmpegPath: tools.FFmpegPath,
		JSRuntime:  tools.JSRuntime,
		OnProgress: func(pct int, detail, speed, eta string) {
			s.updateJob(j, 10+(pct*35)/100, "Baixando vídeo: "+detail, speed, eta)
		},
	}
	dlDir := filepath.Join(s.cacheDir, "dl_"+key)
	p, err := youtube.DownloadVideo(dlOpts, src, dlDir)
	if err != nil {
		return "", err
	}
	// Move into stable cache path
	ext := filepath.Ext(p)
	if ext == "" {
		ext = ".mp4"
	}
	stable := filepath.Join(s.cacheDir, "vid_"+key+ext)
	_ = os.Remove(stable)
	if os.Rename(p, stable) == nil {
		_ = os.RemoveAll(dlDir)
		return stable, nil
	}
	return p, nil
}

// MountVideoSite decodes a single optical video (or auto-discovers local multi-shard siblings) and mounts its files into RAM.
func (s *Server) MountVideoSite(videoPath, origSource, subPath, displayAddr string, passwords []string, j *BrowserJob) (*MountedSite, int, error) {
	return s.MountMultiVideoSite([]string{videoPath}, origSource, subPath, displayAddr, passwords, j)
}

// MountMultiVideoSite decodes 1 or N parallel sharded optical videos (YTSH001), verifies SHA-256, and mounts the site into RAM.
func (s *Server) MountMultiVideoSite(videoPaths []string, origSource, subPath, displayAddr string, passwords []string, j *BrowserJob) (*MountedSite, int, error) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		return nil, 0, errors.New("FFmpeg não encontrado — necessário para decodificar o vídeo óptico")
	}
	if len(videoPaths) == 0 {
		return nil, 0, errors.New("nenhum vídeo fornecido")
	}

	var (
		effectiveManifest []byte
		effectivePayload  []byte
		resWidth          int
		resHeight         int
		profileCode       string
		totalFrames       int
		totalCorrected    int
		shardCount        int
	)

	decodeOne := func(vp string, progFn func(fr, tot int)) (*codec.VideoDecodeResult, error) {
		return codec.DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, vp, false, progFn)
	}

	// If single video provided, decode it first; if it's a YTSH001 shard, auto-discover local siblings!
	if len(videoPaths) == 1 {
		if j != nil {
			s.updateJob(j, 48, "Decodificando frames ópticos (Multi-Core + Reed-Solomon FEC)...", "", "")
		}
		decRes, err := decodeOne(videoPaths[0], func(fr, tot int) {
			if j != nil && tot > 0 {
				s.updateJob(j, 48+(fr*38)/tot, fmt.Sprintf("Decodificando frame óptico %d/%d...", fr, tot), "", "")
			}
		})
		if err != nil {
			return nil, 0, fmt.Errorf("este vídeo não contém um site óptico BrightWeb/BrightVault válido: %w", err)
		}
		if isShard, shInfo, sErr := crypto.ParseShardManifest(decRes.EncryptedManifest); isShard {
			if sErr != nil {
				return nil, 0, sErr
			}
			// Auto-discover sibling files in the same local folder!
			siblings := discoverLocalShardSiblings(videoPaths[0], shInfo.ShardCount)
			if len(siblings) != shInfo.ShardCount {
				return nil, 0, fmt.Errorf("este vídeo é a parte %d/%d de um site Multi-Vídeo (YTSH001) — informe todos os %d links separados por '+' ou selecione todos os %d arquivos locais", shInfo.ShardIndex+1, shInfo.ShardCount, shInfo.ShardCount, shInfo.ShardCount)
			}
			// Decode remaining sibling shards in parallel!
			videoPaths = siblings
			if j != nil {
				s.updateJob(j, 62, fmt.Sprintf("Site Multi-Vídeo detectado (%d partes) — decodificando partes restantes em paralelo...", shInfo.ShardCount), "", "")
			}
			type shardDec struct {
				res *codec.VideoDecodeResult
				err error
			}
			results := make([]shardDec, len(videoPaths))
			var wg sync.WaitGroup
			for i, vp := range videoPaths {
				if filepath.Clean(vp) == filepath.Clean(siblings[shInfo.ShardIndex]) {
					results[i] = shardDec{res: decRes}
					continue
				}
				wg.Add(1)
				go func(idx int, path string) {
					defer wg.Done()
					r, e := decodeOne(path, nil)
					results[idx] = shardDec{res: r, err: e}
				}(i, vp)
			}
			wg.Wait()
			allRes := make([]*codec.VideoDecodeResult, 0, len(results))
			for i, r := range results {
				if r.err != nil {
					return nil, 0, fmt.Errorf("falha decodificando parte %d/%d: %w", i+1, len(results), r.err)
				}
				allRes = append(allRes, r.res)
			}
			var reErr error
			effectiveManifest, effectivePayload, shardCount, resWidth, resHeight, profileCode, totalFrames, totalCorrected, reErr = reassembleShards(allRes)
			if reErr != nil {
				return nil, 0, reErr
			}
		} else {
			effectiveManifest = decRes.EncryptedManifest
			effectivePayload = decRes.EncryptedPayload
			resWidth, resHeight = decRes.Width, decRes.Height
			profileCode = decRes.ProfileCode
			totalFrames = decRes.TotalFrames
			totalCorrected = decRes.TotalCorrectedBytes
		}
	} else {
		// Decode ALL N video shards concurrently in parallel!
		n := len(videoPaths)
		if j != nil {
			s.updateJob(j, 48, fmt.Sprintf("Decodificando %d vídeos paralelos simultaneamente (Multi-Core + FEC)...", n), "", "")
		}
		type shardDec struct {
			res *codec.VideoDecodeResult
			err error
		}
		results := make([]shardDec, n)
		var doneCount atomic.Int32
		sem := make(chan struct{}, 4) // up to 4 parallel FFmpeg decoders sharing CPU smoothly
		var wg sync.WaitGroup
		for i, vp := range videoPaths {
			wg.Add(1)
			go func(idx int, path string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				r, e := decodeOne(path, nil)
				results[idx] = shardDec{res: r, err: e}
				done := int(doneCount.Add(1))
				if j != nil {
					s.updateJob(j, 48+(done*38)/n, fmt.Sprintf("Vídeos paralelos decodificados: %d/%d...", done, n), "", "")
				}
			}(i, vp)
		}
		wg.Wait()
		allRes := make([]*codec.VideoDecodeResult, 0, n)
		for i, r := range results {
			if r.err != nil {
				return nil, 0, fmt.Errorf("falha decodificando vídeo %d/%d: %w", i+1, n, r.err)
			}
			allRes = append(allRes, r.res)
		}
		var reErr error
		effectiveManifest, effectivePayload, shardCount, resWidth, resHeight, profileCode, totalFrames, totalCorrected, reErr = reassembleShards(allRes)
		if reErr != nil {
			return nil, 0, reErr
		}
	}

	var (
		manifestJSON []byte
		rawPayload   []byte
		encMode      string
		pwLayers     int
	)

	// 1. Check passwordless Open Optical Web envelope (YTWB001)
	if isOpen, _, mBytes, oErr := crypto.ParseOpenWebManifest(effectiveManifest); isOpen {
		if oErr != nil {
			return nil, 0, oErr
		}
		manifestJSON = mBytes
		rawPayload = effectivePayload
		encMode = "open"
	} else if isPub, layers, salt, contUUID, encManifest, pErr := crypto.ParsePublicManifest(effectiveManifest); isPub {
		// 2. Check Public Post-Quantum Cascade envelope (YTPUB001 — 1..5 passwords)
		if pErr != nil {
			return nil, 0, pErr
		}
		pwLayers = layers
		encMode = "public-pq"
		cleanPWs, _ := crypto.NormalizePublicPasswords(passwords)
		if len(cleanPWs) != layers {
			return nil, layers, nil // Signal UI to prompt for exact number of passwords!
		}
		if j != nil {
			s.updateJob(j, 88, fmt.Sprintf("Descriptografando cascata Pós-Quântica (%d camada(s))...", layers), "", "")
		}
		var err error
		manifestJSON, err = crypto.DecryptPublicCascade(cleanPWs, salt, encManifest, []byte("YTCloud-Public-Manifest:"+contUUID))
		if err != nil {
			return nil, layers, err
		}
		rawPayload, err = crypto.DecryptPublicCascade(cleanPWs, salt, effectivePayload, []byte("YTCloud-Public-Payload:"+contUUID))
		if err != nil {
			return nil, layers, err
		}
	} else {
		return nil, 0, errors.New("este vídeo foi criptografado para um Cofre privado do YTCloud (use Modo Público ou YTBrowser Publicar Site para abrir aqui)")
	}

	manifest, err := packer.UnmarshalManifest(manifestJSON)
	if err != nil {
		return nil, 0, fmt.Errorf("manifesto do site corrompido: %w", err)
	}
	if j != nil {
		s.updateJob(j, 94, "Descomprimindo arquivos do site (Solid Zstd + SHA-256) na RAM...", "", "")
	}
	memFiles, err := packer.ExtractAllToMemory(manifest, rawPayload, tools.FFmpegPath)
	if err != nil {
		return nil, 0, err
	}

	// Normalize common top-level directory prefix (e.g. "meusite/index.html" -> "index.html")
	normFiles, rootPrefix := normalizeSiteFileTree(memFiles)
	entryPath := detectEntryPath(normFiles, subPath)
	title := extractHTMLTitle(normFiles[entryPath])
	if title == "" {
		if ytID := youtube.ExtractVideoID(origSource); ytID != "" {
			title = "BrightWeb · " + ytID
		} else {
			title = filepath.Base(origSource)
		}
	}

	var fileList []SiteFileSummary
	var totalBytes int64
	for p, data := range normFiles {
		totalBytes += int64(len(data))
		mt := detectMime(p, data)
		fileList = append(fileList, SiteFileSummary{
			Path:     p,
			Size:     int64(len(data)),
			MimeType: mt,
			SHA256:   crypto.SHA256Hex(data)[:16],
		})
	}
	sort.Slice(fileList, func(i, j int) bool { return fileList[i].Path < fileList[j].Path })

	siteID, _ := crypto.RandomHex(6)
	site := &MountedSite{
		ID:             siteID,
		Source:         origSource,
		DisplayAddress: displayAddr,
		EntryPath:      entryPath,
		Title:          title,
		EncryptionMode: encMode,
		PasswordLayers: pwLayers,
		Resolution:     fmt.Sprintf("%dx%d", resWidth, resHeight),
		ProfileCode:    profileCode,
		ShardCount:     shardCount,
		TotalFrames:    totalFrames,
		DecodedFrames:  totalFrames,
		CorrectedBytes: totalCorrected,
		TotalBytes:     totalBytes,
		LoadedFiles:    len(normFiles),
		TotalFiles:     len(normFiles),
		FileList:       fileList,
		DecodedAt:      time.Now().Format(time.RFC3339),
		VideoLocalPath: videoPaths[0],
		RootPrefix:     rootPrefix,
		Files:          normFiles,
	}
	site.cond = sync.NewCond(&site.mu)

	s.mu.Lock()
	s.sites[siteID] = site
	s.mu.Unlock()
	return site, 0, nil
}

func reassembleShards(decoded []*codec.VideoDecodeResult) (innerManifest, payload []byte, shardCount, w, h int, profileCode string, totalFrames, totalCorrected int, err error) {
	if len(decoded) < 2 {
		return nil, nil, 0, 0, 0, "", 0, 0, errors.New("mínimo de 2 shards necessários")
	}
	var (
		groupID      string
		expectedCnt  int
		expectedLen  uint64
		expectedHash [32]byte
	)
	byIdx := map[int]*codec.VideoDecodeResult{}
	for i, d := range decoded {
		isShard, info, pErr := crypto.ParseShardManifest(d.EncryptedManifest)
		if !isShard || pErr != nil {
			return nil, nil, 0, 0, 0, "", 0, 0, fmt.Errorf("o vídeo %d/%d não é uma parte válida de site Multi-Vídeo (YTSH001)", i+1, len(decoded))
		}
		if groupID == "" {
			groupID = info.GroupID
			expectedCnt = info.ShardCount
			expectedLen = info.TotalPayloadLen
			expectedHash = info.TotalPayloadSHA256
			w, h, profileCode = d.Width, d.Height, d.ProfileCode
		} else {
			if info.GroupID != groupID || info.ShardCount != expectedCnt {
				return nil, nil, 0, 0, 0, "", 0, 0, fmt.Errorf("o vídeo %d/%d pertence a outro site/grupo diferente (%s != %s)", i+1, len(decoded), info.GroupID, groupID)
			}
		}
		if info.ShardIndex == 0 && len(info.InnerManifest) > 0 {
			innerManifest = info.InnerManifest
		}
		byIdx[info.ShardIndex] = d
		totalFrames += d.TotalFrames
		totalCorrected += d.TotalCorrectedBytes
	}
	if len(byIdx) < expectedCnt {
		var missing []string
		for i := 0; i < expectedCnt; i++ {
			if _, ok := byIdx[i]; !ok {
				missing = append(missing, fmt.Sprintf("%d/%d", i+1, expectedCnt))
			}
		}
		return nil, nil, 0, 0, 0, "", 0, 0, fmt.Errorf("partes ausentes do site Multi-Vídeo: %s", strings.Join(missing, ", "))
	}
	if len(innerManifest) == 0 {
		return nil, nil, 0, 0, 0, "", 0, 0, errors.New("manifesto principal ausente na parte 1")
	}
	var buf bytes.Buffer
	buf.Grow(int(expectedLen))
	for i := 0; i < expectedCnt; i++ {
		buf.Write(byIdx[i].EncryptedPayload)
	}
	payload = buf.Bytes()
	if uint64(len(payload)) != expectedLen {
		return nil, nil, 0, 0, 0, "", 0, 0, fmt.Errorf("tamanho do payload remontado inválido (%d != %d)", len(payload), expectedLen)
	}
	actualHash := sha256.Sum256(payload)
	if actualHash != expectedHash {
		return nil, nil, 0, 0, 0, "", 0, 0, errors.New("verificação SHA-256 do site Multi-Vídeo falhou (alguma parte está corrompida)")
	}
	return innerManifest, payload, expectedCnt, w, h, profileCode, totalFrames, totalCorrected, nil
}

func normalizeSiteFileTree(raw map[string][]byte) (map[string][]byte, string) {
	if len(raw) == 0 {
		return raw, ""
	}
	// If index.html already exists at root, keep as-is!
	for k := range raw {
		if strings.EqualFold(k, "index.html") || strings.EqualFold(k, "index.htm") {
			return raw, ""
		}
	}
	// Check if all files share a single top-level directory (e.g. "site/index.html", "site/style.css")
	var firstDir string
	allShare := true
	for k := range raw {
		idx := strings.IndexByte(k, '/')
		if idx <= 0 {
			allShare = false
			break
		}
		dir := k[:idx+1]
		if firstDir == "" {
			firstDir = dir
		} else if firstDir != dir {
			allShare = false
			break
		}
	}
	if !allShare || firstDir == "" {
		return raw, ""
	}
	out := make(map[string][]byte, len(raw))
	for k, v := range raw {
		out[strings.TrimPrefix(k, firstDir)] = v
	}
	return out, firstDir
}

func detectEntryPath(files map[string][]byte, requested string) string {
	if requested != "" {
		req := strings.TrimPrefix(path.Clean("/"+requested), "/")
		if _, ok := files[req]; ok {
			return req
		}
	}
	for _, cand := range []string{"index.html", "index.htm", "Index.html", "INDEX.HTML"} {
		if _, ok := files[cand]; ok {
			return cand
		}
	}
	// Fallback: any first .html file at shallowest depth
	var htmls []string
	for k := range files {
		lk := strings.ToLower(k)
		if strings.HasSuffix(lk, ".html") || strings.HasSuffix(lk, ".htm") {
			htmls = append(htmls, k)
		}
	}
	if len(htmls) > 0 {
		sort.Slice(htmls, func(i, j int) bool {
			di, dj := strings.Count(htmls[i], "/"), strings.Count(htmls[j], "/")
			if di != dj {
				return di < dj
			}
			return htmls[i] < htmls[j]
		})
		return htmls[0]
	}
	return "" // Triggers auto-generated optical directory index!
}

func extractHTMLTitle(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	s := string(b)
	low := strings.ToLower(s)
	i := strings.Index(low, "<title>")
	if i < 0 {
		return ""
	}
	j := strings.Index(low[i+7:], "</title>")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(s[i+7 : i+7+j]))
}

func detectMime(p string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(p))
	switch ext {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".wasm":
		return "application/wasm"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".ico":
		return "image/x-icon"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".ttf":
		return "font/ttf"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	}
	if mt := mime.TypeByExtension(ext); mt != "" {
		return mt
	}
	return http.DetectContentType(data)
}

func (s *Server) handleNavigate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		Address     string   `json:"address"`
		Passwords   []string `json:"passwords"`
		ForceReload bool     `json:"force_reload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Address) == "" {
		writeError(w, 400, "informe um link de vídeo (YouTube, btweb:// ou .mp4 local)")
		return
	}

	resolvedAddr, dnsPWs, dnsDisp, dnsHit := s.resolveDNSAddress(req.Address)
	effPWs := req.Passwords
	if len(effPWs) == 0 && len(dnsPWs) > 0 {
		effPWs = dnsPWs
	}
	videoSrcs, subPath, dispAddr := ParseMultiAddress(resolvedAddr)
	if dnsHit && dnsDisp != "" {
		dispAddr = dnsDisp
	}
	if len(videoSrcs) == 0 {
		writeError(w, 400, "informe um link de vídeo (YouTube, btweb://, domínio da Minha Lista ou .mp4 local)")
		return
	}
	joinedSrc := strings.Join(videoSrcs, "|")
	pwHash := sha256.Sum256([]byte(strings.Join(effPWs, "\x00")))
	cacheKey := joinedSrc + "|" + hex.EncodeToString(pwHash[:8])

	if !req.ForceReload {
		s.mu.RLock()
		if sid, ok := s.siteByKey[cacheKey]; ok {
			if site, exists := s.sites[sid]; exists {
				s.mu.RUnlock()
				site.mu.RLock()
				entry := detectEntryPath(site.Files, subPath)
				site.mu.RUnlock()
				cp := site.snapshotJSON()
				if entry != "" {
					cp.EntryPath = entry
				}
				writeJSON(w, map[string]any{"cached": true, "site": &cp})
				return
			}
		}
		s.mu.RUnlock()
	}

	job := s.createJob("navigate", "Preparando navegação óptica...")
	go func() {
		// Fast-path: Progressive Layer-by-Layer Streaming (Direct Online YouTube/HTTP stream WITHOUT saving to disk,
		// or progressive local streaming — opens Camada 0 index.html/CSS/JS live while Camadas 1..N stream in background!)
		if len(videoSrcs) == 1 && len(effPWs) == 0 {
			site, shouldFallback, streamErr := s.tryMountProgressiveStream(videoSrcs[0], subPath, dispAddr, cacheKey, job)
			if site != nil {
				snap := site.snapshotJSON()
				s.finishJob(job, "done", &snap, nil)
				return
			}
			if !shouldFallback && streamErr != nil {
				s.finishJob(job, "error", nil, streamErr)
				return
			}
		}

		// Download/resolve all video shards in parallel (accelerated with yt-dlp -N 8)!
		videoPaths := make([]string, len(videoSrcs))
		if len(videoSrcs) == 1 {
			vp, err := s.resolveOrDownloadVideo(videoSrcs[0], job)
			if err != nil {
				s.finishJob(job, "error", nil, err)
				return
			}
			videoPaths[0] = vp
		} else {
			s.updateJob(job, 8, fmt.Sprintf("Baixando %d vídeos paralelos simultaneamente...", len(videoSrcs)), "", "")
			var (
				wg       sync.WaitGroup
				errMu    sync.Mutex
				firstErr error
				dlDone   atomic.Int32
			)
			for i, src := range videoSrcs {
				wg.Add(1)
				go func(idx int, u string) {
					defer wg.Done()
					vp, err := s.resolveOrDownloadVideo(u, nil)
					if err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("parte %d/%d: %w", idx+1, len(videoSrcs), err)
						}
						errMu.Unlock()
						return
					}
					videoPaths[idx] = vp
					d := int(dlDone.Add(1))
					s.updateJob(job, 10+(d*34)/len(videoSrcs), fmt.Sprintf("Baixando vídeos paralelos: %d/%d prontos...", d, len(videoSrcs)), "", "")
				}(i, src)
			}
			wg.Wait()
			if firstErr != nil {
				s.finishJob(job, "error", nil, firstErr)
				return
			}
		}
		site, needLayers, err := s.MountMultiVideoSite(videoPaths, joinedSrc, subPath, dispAddr, effPWs, job)
		if err != nil {
			s.mu.Lock()
			job.NeedLayers = needLayers
			job.CachedVideo = videoPaths[0]
			s.mu.Unlock()
			s.finishJob(job, "error", nil, err)
			return
		}
		if needLayers > 0 && site == nil {
			s.mu.Lock()
			job.NeedLayers = needLayers
			job.CachedVideo = videoPaths[0]
			s.mu.Unlock()
			s.finishJob(job, "needs_password", map[string]any{
				"password_layers": needLayers,
				"address":         dispAddr,
			}, nil)
			return
		}
		s.mu.Lock()
		s.siteByKey[cacheKey] = site.ID
		s.mu.Unlock()
		snap := site.snapshotJSON()
		s.finishJob(job, "done", &snap, nil)
	}()

	writeJSON(w, map[string]any{"cached": false, "job": job})
}

func detectManifestRootPrefix(files []packer.PackedFileEntry) string {
	if len(files) == 0 {
		return ""
	}
	cleanPaths := make([]string, 0, len(files))
	for _, f := range files {
		p := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(f.RelPath))), "/")
		if strings.EqualFold(p, "index.html") || strings.EqualFold(p, "index.htm") {
			return ""
		}
		cleanPaths = append(cleanPaths, p)
	}
	var firstDir string
	for _, p := range cleanPaths {
		idx := strings.IndexByte(p, '/')
		if idx <= 0 {
			return ""
		}
		dir := p[:idx+1]
		if firstDir == "" {
			firstDir = dir
		} else if firstDir != dir {
			return ""
		}
	}
	return firstDir
}

func detectExpectedEntryPath(expected map[string]bool, requested string) string {
	if requested != "" {
		req := strings.TrimPrefix(path.Clean("/"+requested), "/")
		if expected[req] {
			return req
		}
	}
	for _, cand := range []string{"index.html", "index.htm", "Index.html", "INDEX.HTML"} {
		if expected[cand] {
			return cand
		}
	}
	var htmls []string
	for k := range expected {
		lk := strings.ToLower(k)
		if strings.HasSuffix(lk, ".html") || strings.HasSuffix(lk, ".htm") {
			htmls = append(htmls, k)
		}
	}
	if len(htmls) > 0 {
		sort.Slice(htmls, func(i, j int) bool {
			di, dj := strings.Count(htmls[i], "/"), strings.Count(htmls[j], "/")
			if di != dj {
				return di < dj
			}
			return htmls[i] < htmls[j]
		})
		return htmls[0]
	}
	return ""
}

// tryMountProgressiveStream attempts zero-disk direct online streaming (YouTube / HTTP) or
// progressive local streaming for Open Web (YTWB001) sites, mounting Camada 0 (index.html + CSS/JS)
// immediately while Camadas 1..N decode in the background.
// Returns (site, shouldFallback, err). When shouldFallback is true, caller falls back to full download + MountMultiVideoSite.
func (s *Server) tryMountProgressiveStream(origSrc, subPath, dispAddr, cacheKey string, j *BrowserJob) (*MountedSite, bool, error) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		return nil, true, nil
	}

	var (
		streamInput string
		knownW      int
		knownH      int
		streamMode  string
		localPath   string
		isOnline    bool
	)

	if !isRemoteURL(origSrc) {
		if _, err := os.Stat(origSrc); err != nil {
			return nil, false, fmt.Errorf("arquivo de vídeo não encontrado: %s", origSrc)
		}
		streamInput = origSrc
		localPath = origSrc
		streamMode = "local-live"
	} else if cached := s.findCachedVideo(origSrc); cached != "" {
		streamInput = cached
		localPath = cached
		streamMode = "local-live"
		s.updateJob(j, 25, "Abrindo vídeo do cache em modo progressivo (Camada 0 primeiro)...", "", "")
	} else {
		lowerPath := strings.ToLower(strings.SplitN(origSrc, "?", 2)[0])
		if strings.HasSuffix(lowerPath, ".mp4") || strings.HasSuffix(lowerPath, ".mkv") || strings.HasSuffix(lowerPath, ".webm") {
			streamInput = origSrc
			streamMode = "online-live"
			isOnline = true
			s.updateJob(j, 14, "Conectando stream HTTP direto (assistindo online sem baixar)...", "", "")
		} else {
			if !tools.HasYtDlp {
				return nil, true, nil
			}
			s.updateJob(j, 10, "Resolvendo stream online direto (sem baixar vídeo pro disco)...", "", "")
			info, err := youtube.ResolveDirectStreamURL(youtube.YtDlpOptions{
				YtDlpPath:  tools.YtDlpPath,
				FFmpegPath: tools.FFmpegPath,
				JSRuntime:  tools.JSRuntime,
			}, origSrc)
			if err != nil || info == nil || info.StreamURL == "" {
				// Clean fallback to parallel yt-dlp download (-N 8)
				return nil, true, err
			}
			streamInput = info.StreamURL
			knownW = info.Width
			knownH = info.Height
			streamMode = "online-live"
			isOnline = true
			s.updateJob(j, 22, fmt.Sprintf("Assistindo stream online %dx%d ao vivo (carregando Camada 0)...", knownW, knownH), "", "")
		}
	}

	type firstPaintSignal struct {
		site           *MountedSite
		shouldFallback bool
		err            error
	}
	firstPaintCh := make(chan firstPaintSignal, 1)
	var firstPaintOnce sync.Once
	sendFirstPaint := func(sig firstPaintSignal) {
		firstPaintOnce.Do(func() {
			firstPaintCh <- sig
		})
	}

	go func() {
		var (
			site        *MountedSite
			unpacker    *packer.ProgressiveUnpacker
			rootPrefix  string
			layer0Paths map[string]bool
			notOpenWeb  bool
			allFromCAS  bool
			mountedLive bool
		)
		defer func() {
			if unpacker != nil {
				unpacker.Close()
			}
		}()

		addUnlockedFiles := func(unlocked map[string][]byte, persistCAS bool) {
			if site == nil || len(unlocked) == 0 {
				return
			}
			site.mu.Lock()
			for cleanRel, raw := range unlocked {
				normPath := strings.TrimPrefix(cleanRel, rootPrefix)
				if _, exists := site.Files[normPath]; exists {
					continue
				}
				site.Files[normPath] = raw
				site.TotalBytes += int64(len(raw))
				site.LoadedFiles = len(site.Files)
				mt := detectMime(normPath, raw)
				shaHex := crypto.SHA256Hex(raw)
				site.FileList = append(site.FileList, SiteFileSummary{
					Path:     normPath,
					Size:     int64(len(raw)),
					MimeType: mt,
					SHA256:   shaHex[:16],
				})
				if persistCAS && s.cas != nil {
					rawCopy := raw
					go s.cas.Put(shaHex, rawCopy)
				}
			}
			sort.Slice(site.FileList, func(i, j int) bool { return site.FileList[i].Path < site.FileList[j].Path })
			if site.EntryPath != "" {
				if htmlData, ok := site.Files[site.EntryPath]; ok {
					if t := extractHTMLTitle(htmlData); t != "" {
						site.Title = t
					}
				}
			}
			// Check whether Camada 0 (EntryPath + all root-depth files) is now ready for instant First Paint!
			canPaint := false
			if !mountedLive {
				if len(layer0Paths) > 0 {
					allL0 := true
					for p := range layer0Paths {
						if _, ok := site.Files[p]; !ok {
							allL0 = false
							break
						}
					}
					canPaint = allL0
				} else if len(site.Files) > 0 {
					canPaint = true
				}
			}
			site.cond.Broadcast()
			site.mu.Unlock()

			if canPaint {
				mountedLive = true
				s.mu.Lock()
				s.sites[site.ID] = site
				if cacheKey != "" {
					s.siteByKey[cacheKey] = site.ID
				}
				s.mu.Unlock()
				sendFirstPaint(firstPaintSignal{site: site})
			}
		}

		hooks := codec.ProgressiveHooks{
			OnManifest: func(encManifest []byte, totalFrames int, profileCode string, width, height int, cfSpec codec.CrossFrameSpec) bool {
				isOpen, _, mBytes, oErr := crypto.ParseOpenWebManifest(encManifest)
				if !isOpen || oErr != nil {
					notOpenWeb = true
					return false // Stop immediately -> let MountMultiVideoSite handle YTPUB001 / YTSH001!
				}
				manifest, mErr := packer.UnmarshalManifest(mBytes)
				if mErr != nil {
					notOpenWeb = true
					return false
				}
				upk, uErr := packer.NewProgressiveUnpacker(manifest, tools.FFmpegPath)
				if uErr != nil {
					notOpenWeb = true
					return false
				}
				unpacker = upk
				rootPrefix = detectManifestRootPrefix(manifest.Files)
				expected := make(map[string]bool, len(manifest.Files))
				layer0Paths = make(map[string]bool)
				for _, f := range manifest.Files {
					cleanRel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(f.RelPath))), "/")
					normPath := strings.TrimPrefix(cleanRel, rootPrefix)
					expected[normPath] = true
					if f.LayerDepth == 0 {
						layer0Paths[normPath] = true
					}
				}
				entryPath := detectExpectedEntryPath(expected, subPath)
				if entryPath != "" {
					layer0Paths[entryPath] = true
				}
				// Limit first-paint blocking to small root assets (<= 512 KB) so a huge root media file never delays index.html!
				for _, f := range manifest.Files {
					cleanRel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(f.RelPath))), "/")
					normPath := strings.TrimPrefix(cleanRel, rootPrefix)
					if normPath != entryPath && f.OriginalSize > 512*1024 {
						delete(layer0Paths, normPath)
					}
				}
				title := filepath.Base(origSrc)
				if ytID := youtube.ExtractVideoID(origSrc); ytID != "" {
					title = "BrightWeb · " + ytID
				}
				siteID, _ := crypto.RandomHex(6)
				site = &MountedSite{
					ID:             siteID,
					Source:         origSrc,
					DisplayAddress: dispAddr,
					EntryPath:      entryPath,
					Title:          title,
					EncryptionMode: "open",
					Resolution:     fmt.Sprintf("%dx%d", width, height),
					ProfileCode:    profileCode,
					TotalFrames:    totalFrames,
					Streaming:      true,
					StreamMode:     streamMode,
					TotalFiles:     len(expected),
					DecodedAt:      time.Now().Format(time.RFC3339),
					VideoLocalPath: localPath,
					RootPrefix:     rootPrefix,
					Files:          make(map[string][]byte, len(expected)),
					expectedFiles:  expected,
				}
				site.cond = sync.NewCond(&site.mu)

				// BrightNet/3 SHA-256 Content-Addressed Storage (CAS / ETag):
				// Pre-load any files already present & verified on disk in 0 ms!
				// If 100% of files hit CAS cache, abort video stream immediately after manifest frame 0!
				if s.cas != nil {
					casUnlocked := make(map[string][]byte)
					for _, f := range manifest.Files {
						if f.SHA256 == "" {
							continue
						}
						if cachedBytes, ok := s.cas.Get(f.SHA256); ok {
							unpacker.MarkExtracted(f.RelPath)
							cleanRel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(f.RelPath))), "/")
							casUnlocked[cleanRel] = cachedBytes
						}
					}
					if len(casUnlocked) > 0 {
						site.mu.Lock()
						site.CASCacheHits = len(casUnlocked)
						site.mu.Unlock()
						addUnlockedFiles(casUnlocked, false)
						if len(casUnlocked) >= len(manifest.Files) && len(manifest.Files) > 0 {
							allFromCAS = true
							site.mu.Lock()
							site.StreamMode = "cas-instant"
							site.Streaming = false
							site.cond.Broadcast()
							site.mu.Unlock()
							return false // 100% satisfied by CAS cache in 0 ms — kill video stream immediately!
						}
					}
				}

				if j != nil && !mountedLive {
					s.updateJob(j, 42, fmt.Sprintf("Manifesto lido (%d arquivos) — carregando Camada 0 ao vivo...", len(expected)), "", "")
				}
				return true
			},
			OnContiguous: func(contiguousPayload []byte, decodedFrames, totalFrames int) {
				if site == nil || unpacker == nil {
					return
				}
				site.mu.Lock()
				site.DecodedFrames = decodedFrames
				if totalFrames > 0 {
					site.TotalFrames = totalFrames
				}
				site.mu.Unlock()
				unlocked, uErr := unpacker.Feed(contiguousPayload)
				if uErr == nil && len(unlocked) > 0 {
					addUnlockedFiles(unlocked, true)
				}
			},
			OnSparseFrame: func(seenDataFrames map[uint32][]byte, startDataIdx uint32, frameChunkSize int, decodedFrames, totalFrames int) {
				if site == nil || unpacker == nil {
					return
				}
				// BrightNet/3 HTTP/3-style Zero Head-of-Line Blocking:
				// Unlock any file/layer whose specific frames arrived, even past a missing frame!
				unlocked, uErr := unpacker.FeedSparse(seenDataFrames, startDataIdx, frameChunkSize)
				if uErr == nil && len(unlocked) > 0 {
					addUnlockedFiles(unlocked, true)
				}
			},
			OnProgress: func(decodedFrames, totalFrames int) {
				if site != nil {
					site.mu.Lock()
					site.DecodedFrames = decodedFrames
					if totalFrames > 0 {
						site.TotalFrames = totalFrames
					}
					site.mu.Unlock()
				}
				if j != nil && !mountedLive && totalFrames > 0 {
					pct := 42 + (decodedFrames*50)/totalFrames
					if pct > 95 {
						pct = 95
					}
					modeTxt := "Decodificando"
					if isOnline {
						modeTxt = "Assistindo online"
					}
					s.updateJob(j, pct, fmt.Sprintf("%s frame óptico %d/%d...", modeTxt, decodedFrames, totalFrames), "", "")
				}
			},
		}

		decRes, decErr := codec.DecodeVideoStreamProgressive(tools.FFmpegPath, tools.FFprobePath, streamInput, knownW, knownH, hooks)
		if allFromCAS && site != nil {
			if !mountedLive {
				mountedLive = true
				s.mu.Lock()
				s.sites[site.ID] = site
				if cacheKey != "" {
					s.siteByKey[cacheKey] = site.ID
				}
				s.mu.Unlock()
				sendFirstPaint(firstPaintSignal{site: site})
			}
			return
		}
		if notOpenWeb {
			sendFirstPaint(firstPaintSignal{shouldFallback: true})
			return
		}
		if decErr != nil {
			if site != nil {
				site.mu.Lock()
				site.Streaming = false
				site.StreamError = decErr.Error()
				site.cond.Broadcast()
				site.mu.Unlock()
			}
			if !mountedLive {
				// Online direct stream failed before first paint -> seamlessly fall back to parallel yt-dlp download!
				if isOnline {
					sendFirstPaint(firstPaintSignal{shouldFallback: true, err: decErr})
				} else if knownW == 3840 && knownH == 2160 {
					sendFirstPaint(firstPaintSignal{shouldFallback: true, err: decErr})
				} else {
					sendFirstPaint(firstPaintSignal{shouldFallback: false, err: fmt.Errorf("este vídeo não contém um site óptico BrightWeb/BrightVault válido: %w", decErr)})
				}
			}
			return
		}

		// Stream completed cleanly (and Cross-Frame RS healed any missing frames if active!).
		// Feed the complete reconstructed payload to unlock any remaining files!
		if site != nil && unpacker != nil && len(decRes.EncryptedPayload) > 0 {
			if unlocked, uErr := unpacker.Feed(decRes.EncryptedPayload); uErr == nil && len(unlocked) > 0 {
				addUnlockedFiles(unlocked, true)
			}
		}
		if site != nil {
			site.mu.Lock()
			site.TotalFrames = decRes.TotalFrames
			site.DecodedFrames = decRes.DecodedFrames
			site.CorrectedBytes = decRes.TotalCorrectedBytes
			site.Streaming = false
			site.cond.Broadcast()
			site.mu.Unlock()

			if !mountedLive {
				mountedLive = true
				s.mu.Lock()
				s.sites[site.ID] = site
				if cacheKey != "" {
					s.siteByKey[cacheKey] = site.ID
				}
				s.mu.Unlock()
				sendFirstPaint(firstPaintSignal{site: site})
			}
		} else {
			sendFirstPaint(firstPaintSignal{shouldFallback: true})
		}
	}()

	sig := <-firstPaintCh
	return sig.site, sig.shouldFallback, sig.err
}

const ytwebBridgeScript = `<script>
(function(){
  try {
    if (document.title) window.parent.postMessage({type:'btweb-title', title: document.title}, '*');
    document.addEventListener('click', function(e){
      var a = e.target && e.target.closest ? e.target.closest('a[href]') : null;
      if (!a) return;
      var href = a.getAttribute('href') || '';
      if (/^(btweb|ytweb):\/\//i.test(href) || a.hasAttribute('data-btweb') || a.hasAttribute('data-ytweb')) {
        e.preventDefault();
        window.parent.postMessage({type:'btweb-navigate', url: href}, '*');
      }
    }, true);
  } catch(_) {}
})();
</script>`

// handleServeSite serves any decoded file from RAM for /site/<siteID>/<path...>
func (s *Server) handleServeSite(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/site/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	siteID := parts[0]
	reqPath := ""
	if len(parts) == 2 {
		reqPath = strings.TrimPrefix(path.Clean("/"+parts[1]), "/")
	}

	s.mu.RLock()
	site, ok := s.sites[siteID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "Site óptico expirado ou não encontrado na memória.", http.StatusNotFound)
		return
	}

	// White-hat isolation headers: CORS allowed for opaque sandbox origin, optional strict offline CSP!
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	if s.isolatedMode.Load() {
		// Sandboxed iframes without allow-same-origin run with an opaque origin ("null"), so 'self'
		// alone does not match sibling assets under /site/<siteID>/. Scoping strictly to this site's
		// RAM prefix allows multi-file JS/CSS/WebAssembly/WAD apps while still blocking ALL external
		// internet requests and /api/*!
		sitePrefix := fmt.Sprintf("http://%s/site/%s/", r.Host, siteID)
		w.Header().Set("Content-Security-Policy",
			"default-src 'self' "+sitePrefix+" 'unsafe-inline' 'unsafe-eval' 'wasm-unsafe-eval' data: blob:; "+
				"script-src 'self' "+sitePrefix+" 'unsafe-inline' 'unsafe-eval' 'wasm-unsafe-eval' data: blob:; "+
				"worker-src 'self' "+sitePrefix+" blob: data:; "+
				"connect-src 'self' "+sitePrefix+" data: blob:; "+
				"img-src 'self' "+sitePrefix+" data: blob:; "+
				"media-src 'self' "+sitePrefix+" data: blob:; "+
				"font-src 'self' "+sitePrefix+" data: blob:; "+
				"frame-src 'self' "+sitePrefix+" data: blob:;")
	}

	if reqPath == "" {
		site.mu.RLock()
		reqPath = site.EntryPath
		site.mu.RUnlock()
	}
	// If still empty (no index.html inside container), serve a sleek auto-generated Optical Directory Index!
	if reqPath == "" {
		snap := site.snapshotJSON()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(renderDirectoryIndexHTML(&snap))
		return
	}

	actualPath, data, found := site.waitForFile(r.Context(), reqPath)
	if !found {
		http.NotFound(w, r)
		return
	}
	reqPath = actualPath

	ct := detectMime(reqPath, data)
	w.Header().Set("Content-Type", ct)
	if strings.HasPrefix(ct, "text/html") {
		injected := injectBridge(data)
		_, _ = w.Write(injected)
		return
	}
	_, _ = w.Write(data)
}

func injectBridge(htmlBytes []byte) []byte {
	low := bytes.ToLower(htmlBytes)
	idx := bytes.LastIndex(low, []byte("</body>"))
	if idx >= 0 {
		out := make([]byte, 0, len(htmlBytes)+len(ytwebBridgeScript))
		out = append(out, htmlBytes[:idx]...)
		out = append(out, []byte(ytwebBridgeScript)...)
		out = append(out, htmlBytes[idx:]...)
		return out
	}
	return append(append([]byte{}, htmlBytes...), []byte(ytwebBridgeScript)...)
}

func renderDirectoryIndexHTML(site *MountedSite) []byte {
	var b bytes.Buffer
	b.WriteString(`<!DOCTYPE html><html lang="pt-BR"><head><meta charset="UTF-8"><title>`)
	b.WriteString(html.EscapeString(site.Title))
	b.WriteString(`</title><style>body{font-family:Segoe UI,sans-serif;background:#070b16;color:#e6eefc;padding:28px;margin:0}h1{font-size:20px;color:#38bdf8;margin-bottom:6px}p{color:#8ea2cc;font-size:13px;margin-bottom:16px}table{width:100%;border-collapse:collapse;background:#10182d;border:1px solid #22325a;border-radius:10px;overflow:hidden}th,td{padding:10px 12px;border-bottom:1px solid #1d2b4f;text-align:left;font-size:13px}a{color:#7dd3fc;text-decoration:none;font-weight:600}a:hover{text-decoration:underline}</style></head><body><h1>📂 Índice Óptico — `)
	b.WriteString(html.EscapeString(site.Title))
	b.WriteString(`</h1><p>Este vídeo não possui <code>index.html</code> na raiz. Clique em qualquer arquivo decodificado abaixo para abrir:</p><table><thead><tr><th>Arquivo</th><th>Tipo</th><th>Tamanho</th></tr></thead><tbody>`)
	for _, f := range site.FileList {
		b.WriteString(`<tr><td><a href="`)
		b.WriteString(html.EscapeString(f.Path))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(f.Path))
		b.WriteString(`</a></td><td>`)
		b.WriteString(html.EscapeString(f.MimeType))
		b.WriteString(`</td><td>`)
		b.WriteString(fmt.Sprintf("%d B", f.Size))
		b.WriteString(`</td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	b.WriteString(ytwebBridgeScript)
	b.WriteString(`</body></html>`)
	return b.Bytes()
}

type PublishOptions struct {
	FolderPath          string   `json:"folder_path"`
	OutputPath          string   `json:"output_path"`
	Resolution          string   `json:"resolution"`
	Profile             string   `json:"profile"`
	VideoCodec          string   `json:"video_codec"` // "auto", "h264-mp4", "av1-mp4", "av1-mkv", "vp9-webm", "hevc-mp4"
	MaskIntensity       string   `json:"mask_intensity"`
	ArtTheme            string   `json:"art_theme"`
	ArtKeyword          string   `json:"art_keyword"`
	HostVideoPath       string   `json:"host_video_path"`
	AudioMode           string   `json:"audio_mode"`   // "none", "ambient", "host", "file"
	AudioSource         string   `json:"audio_source"` // local audio file path when AudioMode == "file"
	MusicStyle          string   `json:"music_style"`  // "ambient", "lofi", "synthwave", "meditation", "nature", "piano", "chiptune"
	MusicMood           string   `json:"music_mood"`   // "dreamy", "calm", "melancholic", "energetic", "mysterious"
	MusicInstruments    []string `json:"music_instruments"`
	MusicPrompt         string   `json:"music_prompt"`
	FPS                 int      `json:"fps"`
	HideHeaderBar       bool     `json:"hide_header_bar"`
	Passwords           []string `json:"passwords"` // empty = Open Web (YTWB001), 1..5 = Post-Quantum (YTPUB001)
	UseDemoSite         bool     `json:"use_demo_site"`
	TargetSeconds       int      `json:"target_seconds"`
	DurationSource      string   `json:"duration_source"` // "files", "video", "audio"
	PaddingMode         string   `json:"padding_mode"`    // "auto", "spread", "start"
	OptimizePNGLossless bool     `json:"optimize_png_lossless"`
	Shards              int      `json:"shards"`         // 1 = single video, 2..16 = multi-video parallel sharding (YTSH001)
	CrossFECMode        string   `json:"cross_fec_mode"` // "auto", "max" (K=6,M=2), "high" (K=8,M=2), "off"
	Ctx                 context.Context `json:"-"`
}

type PublishResult struct {
	*codec.VideoEncodeStats
	ShardCount      int      `json:"shard_count"`
	ShardPaths      []string `json:"shard_paths,omitempty"`
	CombinedAddress string   `json:"combined_address,omitempty"`
}

// PublishSiteToVideo encodes a website into 1 or N parallel sharded optical videos.
func (s *Server) PublishSiteToVideo(req PublishOptions, onProgress func(pct int, detail string)) (*PublishResult, error) {
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		return nil, errors.New("FFmpeg não encontrado — necessário para gerar vídeo óptico")
	}

	var inputs []packer.InputFile
	var err error
	if req.UseDemoSite {
		inputs = BuildDemoSiteFiles()
	} else {
		p := strings.Trim(strings.TrimSpace(req.FolderPath), `"`)
		if p == "" {
			return nil, errors.New("selecione a pasta do site (com index.html) ou use o Site Demo")
		}
		inputs, err = packer.CollectInputsFromPath(p)
		if err != nil || len(inputs) == 0 {
			return nil, fmt.Errorf("erro lendo pasta do site: %v", err)
		}
	}

	res := req.Resolution
	if res == "" {
		res = "720p"
	}
	prof := req.Profile
	if prof == "" {
		prof = "art-fullres"
	}
	fps := req.FPS
	if fps <= 0 {
		fps = 15
	}
	audioMode := req.AudioMode
	if audioMode == "" {
		audioMode = "none"
	}

	vCodec := strings.ToLower(strings.TrimSpace(req.VideoCodec))
	outExt := ".mp4"
	switch vCodec {
	case "vp9-webm", "vp9":
		vCodec = "vp9"
		outExt = ".webm"
	case "av1-mkv":
		vCodec = "av1"
		outExt = ".mkv"
	case "av1-mp4", "av1":
		vCodec = "av1"
	case "hevc-mp4", "hevc":
		vCodec = "hevc"
	case "h264-mp4", "h264":
		vCodec = "h264"
	default:
		vCodec = "auto"
	}
	padMode := strings.ToLower(strings.TrimSpace(req.PaddingMode))
	if padMode != "spread" && padMode != "start" {
		padMode = "auto"
	}
	var maskInt uint8 = codec.MaskIntensityDefault
	switch strings.ToLower(strings.TrimSpace(req.MaskIntensity)) {
	case "subtle":
		maskInt = codec.MaskIntensitySubtle
	case "strong":
		maskInt = codec.MaskIntensityStrong
	case "max":
		maskInt = codec.MaskIntensityMax
	}

	outPath := strings.Trim(strings.TrimSpace(req.OutputPath), `"`)
	if outPath == "" {
		home, _ := os.UserHomeDir()
		outDir := filepath.Join(home, "Downloads", "BrightWeb-Sites")
		_ = os.MkdirAll(outDir, 0o755)
		tag := "site"
		if req.UseDemoSite {
			tag = "demo_btweb"
		}
		outPath = filepath.Join(outDir, fmt.Sprintf("%s_%s%s", tag, time.Now().Format("20060102_150405"), outExt))
	}

	if onProgress != nil {
		onProgress(10, fmt.Sprintf("Compactando %d arquivo(s) do site (Hybrid Layer Zstd)...", len(inputs)))
	}
	contUUID, _ := crypto.RandomHex(8)
	salt, _ := crypto.RandomBytes(16)
	contNumericID := binary.LittleEndian.Uint64(salt[:8])
	themeID, mixedSeed := codec.ResolveArtTheme(req.ArtTheme, req.ArtKeyword, contNumericID)

	manifest, rawPayload, err := packer.PackFilesWithOptions(inputs, packer.PackOptions{
		ContainerID:          contUUID,
		ProfileCode:          prof,
		ResolutionCode:       res,
		FFmpegPath:           tools.FFmpegPath,
		IncludeMicroPreviews: false,
		OptimizePNGLossless:  req.OptimizePNGLossless,
	})
	if err != nil {
		return nil, err
	}
	manifest.CreatedAt = time.Now().Format(time.RFC3339)
	manifestJSON, err := packer.MarshalManifest(manifest)
	if err != nil {
		return nil, err
	}

	var wrappedManifest, finalPayload []byte
	cleanPWs, _ := crypto.NormalizePublicPasswords(req.Passwords)
	if len(cleanPWs) > 0 {
		if onProgress != nil {
			onProgress(22, fmt.Sprintf("Criptografando site (%d camada(s) Pós-Quântica)...", len(cleanPWs)))
		}
		encM, err := crypto.EncryptPublicCascade(cleanPWs, salt, manifestJSON, []byte("YTCloud-Public-Manifest:"+contUUID))
		if err != nil {
			return nil, err
		}
		encP, err := crypto.EncryptPublicCascade(cleanPWs, salt, rawPayload, []byte("YTCloud-Public-Payload:"+contUUID))
		if err != nil {
			return nil, err
		}
		wrappedManifest = crypto.WrapPublicManifest(len(cleanPWs), salt, contUUID, encM)
		finalPayload = encP
	} else {
		wrappedManifest = crypto.WrapOpenWebManifest(contUUID, manifestJSON)
		finalPayload = rawPayload
	}

	targetFrames := 0
	if req.TargetSeconds > 0 {
		targetFrames = req.TargetSeconds * fps
	}
	hostClean := strings.Trim(strings.TrimSpace(req.HostVideoPath), `"`)
	audioClean := strings.Trim(strings.TrimSpace(req.AudioSource), `"`)
	durSrc := strings.ToLower(strings.TrimSpace(req.DurationSource))
	if durSrc == "video" && hostClean != "" {
		if durSec, dErr := codec.ProbeMediaDuration(tools.FFprobePath, tools.FFmpegPath, hostClean); dErr == nil && durSec > 0.5 {
			tf := int(durSec * float64(fps))
			if tf > targetFrames {
				targetFrames = tf
			}
		}
	} else if durSrc == "audio" && audioClean != "" {
		if durSec, dErr := codec.ProbeMediaDuration(tools.FFprobePath, tools.FFmpegPath, audioClean); dErr == nil && durSec > 0.5 {
			tf := int(durSec * float64(fps))
			if tf > targetFrames {
				targetFrames = tf
			}
		}
	}
	shards := req.Shards
	if shards < 1 {
		shards = 1
	} else if shards > 16 {
		shards = 16
	}
	musicOpts := codec.MusicOptions{
		Style:       req.MusicStyle,
		Mood:        req.MusicMood,
		Instruments: req.MusicInstruments,
		Prompt:      req.MusicPrompt,
	}

	if shards == 1 {
		if onProgress != nil {
			onProgress(32, "Renderizando vídeo óptico via FFmpeg (Multi-Core)...")
		}
		stats, err := codec.EncodeStreamToVideo(codec.VideoEncodeOptions{
			Ctx:            req.Ctx,
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outPath,
			ResolutionCode: res,
			ProfileCode:    prof,
			VideoCodec:     vCodec,
			FPS:            fps,
			Threads:        0, // multi-core auto!
			LowPriority:    false,
			ContainerID:    mixedSeed,
			ArtTheme:       themeID,
			MaskIntensity:  maskInt,
			HideHeaderBar:  req.HideHeaderBar,
			HostVideoPath:  hostClean,
			AudioMode:      audioMode,
			AudioPath:      audioClean,
			MusicOpts:      musicOpts,
			TargetFrames:   targetFrames,
			PaddingMode:    padMode,
			CrossFECMode:   req.CrossFECMode,
			OnProgress: func(f, tot int) {
				if onProgress != nil && tot > 0 {
					if f >= tot {
						onProgress(97, fmt.Sprintf("Finalizando MP4 (%d/%d frames — movflags faststart)...", f, tot))
					} else {
						onProgress(32+(f*64)/tot, fmt.Sprintf("Renderizando frame %d/%d...", f, tot))
					}
				}
			},
		}, wrappedManifest, finalPayload)
		if err != nil {
			return nil, err
		}
		return &PublishResult{
			VideoEncodeStats: stats,
			ShardCount:       1,
			ShardPaths:       []string{stats.OutputPath},
			CombinedAddress:  stats.OutputPath,
		}, nil
	}

	// Multi-Video Parallel Sharding (YTSH001): split payload evenly into N videos!
	ext := filepath.Ext(outPath)
	if ext == "" {
		ext = ".mp4"
	}
	baseNoExt := strings.TrimSuffix(outPath, ext)
	payloadHash := sha256.Sum256(finalPayload)
	shardPaths := make([]string, shards)
	var (
		totalShardFrames int
		totalShardBytes  int64
		firstStats       *codec.VideoEncodeStats
	)

	for si := 0; si < shards; si++ {
		start := (len(finalPayload) * si) / shards
		end := (len(finalPayload) * (si + 1)) / shards
		partPayload := finalPayload[start:end]
		partPath := fmt.Sprintf("%s_part%dof%d%s", baseNoExt, si+1, shards, ext)
		shardPaths[si] = partPath

		var innerM []byte
		if si == 0 {
			innerM = wrappedManifest
		}
		shardManifest := crypto.WrapShardManifest(crypto.ShardInfo{
			GroupID:            contUUID,
			ShardIndex:         si,
			ShardCount:         shards,
			TotalPayloadLen:    uint64(len(finalPayload)),
			TotalPayloadSHA256: payloadHash,
			InnerManifest:      innerM,
		})

		basePct := 30 + (si*68)/shards
		spanPct := 68 / shards
		if onProgress != nil {
			onProgress(basePct, fmt.Sprintf("Renderizando vídeo paralelo %d/%d...", si+1, shards))
		}
		st, err := codec.EncodeStreamToVideo(codec.VideoEncodeOptions{
			Ctx:            req.Ctx,
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     partPath,
			ResolutionCode: res,
			ProfileCode:    prof,
			VideoCodec:     vCodec,
			FPS:            fps,
			Threads:        0, // multi-core auto!
			LowPriority:    false,
			ContainerID:    mixedSeed ^ uint64(si+1)*0x9E3779B97F4A7C15,
			ArtTheme:       themeID,
			MaskIntensity:  maskInt,
			HideHeaderBar:  req.HideHeaderBar,
			HostVideoPath:  hostClean,
			AudioMode:      audioMode,
			AudioPath:      audioClean,
			MusicOpts:      musicOpts,
			TargetFrames:   targetFrames,
			PaddingMode:    padMode,
			CrossFECMode:   req.CrossFECMode,
			OnProgress: func(f, tot int) {
				if onProgress != nil && tot > 0 {
					p := basePct + (f*spanPct)/tot
					if p > 98 {
						p = 98
					}
					onProgress(p, fmt.Sprintf("Parte %d/%d — frame %d/%d...", si+1, shards, f, tot))
				}
			},
		}, shardManifest, partPayload)
		if err != nil {
			return nil, fmt.Errorf("erro gerando parte %d/%d: %w", si+1, shards, err)
		}
		if firstStats == nil {
			cp := *st
			firstStats = &cp
		}
		totalShardFrames += st.TotalFrames
		totalShardBytes += st.VideoSizeBytes
	}

	firstStats.OutputPath = shardPaths[0]
	firstStats.TotalFrames = totalShardFrames
	firstStats.VideoSizeBytes = totalShardBytes
	return &PublishResult{
		VideoEncodeStats: firstStats,
		ShardCount:       shards,
		ShardPaths:       shardPaths,
		CombinedAddress:  strings.Join(shardPaths, " | "),
	}, nil
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req PublishOptions
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	job := s.createJob("publish", "Empacotando site em vídeo óptico...")
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	job.cancel = cancel
	s.mu.Unlock()
	req.Ctx = ctx
	go func() {
		defer cancel()
		stats, err := s.PublishSiteToVideo(req, func(pct int, detail string) {
			s.updateJob(job, pct, detail, "", "")
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				s.finishJob(job, "cancelled", nil, errors.New("geração cancelada pelo usuário"))
				return
			}
			s.finishJob(job, "error", nil, err)
			return
		}
		s.finishJob(job, "done", stats, nil)
	}()
	writeJSON(w, job)
}

func (s *Server) handlePublishDemo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req PublishOptions
	_ = json.NewDecoder(r.Body).Decode(&req)
	req.UseDemoSite = true
	if req.Resolution == "" {
		req.Resolution = "720p"
	}
	if req.Profile == "" {
		req.Profile = "art-fullres"
	}
	if req.ArtTheme == "" {
		req.ArtTheme = "julia-turing"
	}
	if req.FPS <= 0 {
		req.FPS = 15
	}
	req.HideHeaderBar = true
	if req.TargetSeconds <= 0 {
		req.TargetSeconds = 3 // Quick 3s playable fractal demo video!
	}

	job := s.createJob("publish", "Gerando Site Demo em vídeo fractal...")
	go func() {
		stats, err := s.PublishSiteToVideo(req, func(pct int, detail string) {
			s.updateJob(job, pct, detail, "", "")
		})
		if err != nil {
			s.finishJob(job, "error", nil, err)
			return
		}
		s.finishJob(job, "done", stats, nil)
	}()
	writeJSON(w, job)
}

func (s *Server) handlePickVideo(w http.ResponseWriter, r *http.Request) {
	s.activeDialogs.Add(1)
	defer s.activeDialogs.Add(-1)
	paths, err := sysutil.PickFilesDialog("Selecionar Vídeo Óptico (.mp4 / .mkv / .webm)")
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if len(paths) == 0 {
		writeJSON(w, map[string]any{"cancelled": true})
		return
	}
	writeJSON(w, map[string]any{"cancelled": false, "path": paths[0], "paths": paths})
}

func (s *Server) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	s.activeDialogs.Add(1)
	defer s.activeDialogs.Add(-1)
	dir, err := sysutil.PickFolderDialog("Selecionar Pasta do Site (contendo index.html)")
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if dir == "" {
		writeJSON(w, map[string]any{"cancelled": true})
		return
	}
	writeJSON(w, map[string]any{"cancelled": false, "path": dir})
}

func (s *Server) handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeError(w, 400, "caminho inválido")
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("explorer", "/select,", req.Path).Start()
	}
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

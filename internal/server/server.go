package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
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
	"ytcloud/internal/sysutil"
	"ytcloud/internal/updater"
	"ytcloud/internal/vault"
	"ytcloud/internal/youtube"
)

//go:embed web/*
var webFS embed.FS

type Job struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // "encode", "decode", "peek", "catalog_backup", "tools_install", "calibrate"
	Title     string `json:"title"`
	Status    string `json:"status"`   // "running", "done", "error"
	Progress  int    `json:"progress"` // 0..100
	Detail    string `json:"detail"`
	ETAText   string `json:"eta_text,omitempty"`
	SpeedText string `json:"speed_text,omitempty"`
	Error     string `json:"error,omitempty"`
	Result    any    `json:"result,omitempty"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
}

type Server struct {
	store            *vault.Store
	token            string
	stagingDir       string
	updater          *updater.Manager
	jobsMu           sync.RWMutex
	jobs             []*Job
	mux              *http.ServeMux
	shutdownCh       chan struct{}
	shutdownOnce     sync.Once
	lastHeartbeat    atomic.Int64
	clientConnected  atomic.Bool
	activeSSEClients atomic.Int32
	hadSSEClient     atomic.Bool
	explicitlyLocked atomic.Bool
	activeDialogs    atomic.Int32
}

func New(store *vault.Store) (*Server, error) {
	tok, err := crypto.RandomHex(24)
	if err != nil {
		return nil, err
	}
	staging := filepath.Join(media.DefaultAppDir(), "staging")
	_ = os.MkdirAll(staging, 0o755)

	s := &Server{
		store:      store,
		token:      tok,
		stagingDir: staging,
		updater:    updater.NewManager("BrightVault", "brightbringer", "BrightVault", "BrightVault.exe", media.DefaultAppDir()),
		mux:        http.NewServeMux(),
		shutdownCh: make(chan struct{}),
	}
	// Apply polite default RAM/CPU background limits right at boot
	s.applyResourceLimits(512, 2, true)
	s.registerRoutes()
	s.updater.StartScheduler()
	return s, nil
}

func (s *Server) ShutdownChan() <-chan struct{} { return s.shutdownCh }

func (s *Server) TriggerShutdown() {
	s.shutdownOnce.Do(func() {
		close(s.shutdownCh)
	})
}

func (s *Server) applyResourceLimits(maxRAMMB, cpuThreads int, backgroundMode bool) {
	if maxRAMMB < 256 {
		maxRAMMB = 512
	}
	if cpuThreads < 1 {
		cpuThreads = 2
	}
	debug.SetMemoryLimit(int64(maxRAMMB) * 1024 * 1024)
	debug.SetGCPercent(50)
	codec.SetMaxWorkers(cpuThreads)
	codec.SetResourceBudget(maxRAMMB, backgroundMode)
	sysutil.SetSelfBackgroundPriority(backgroundMode)
}

func (s *Server) applyVaultResourceLimits() {
	maxRAM, threads, bg := 512, 2, true
	_ = s.store.Read(func(d *vault.VaultData) error {
		if d.Settings.MaxRAMMB >= 256 {
			maxRAM = d.Settings.MaxRAMMB
		}
		if d.Settings.CPUThreads >= 1 {
			threads = d.Settings.CPUThreads
		}
		bg = d.Settings.BackgroundMode
		return nil
	})
	s.applyResourceLimits(maxRAM, threads, bg)
}

func (s *Server) hasRunningJobs() bool {
	s.jobsMu.RLock()
	defer s.jobsMu.RUnlock()
	for _, j := range s.jobs {
		if j.Status == "running" {
			return true
		}
	}
	return false
}

func (s *Server) Token() string { return s.token }

func (s *Server) Start(addr string) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	url := fmt.Sprintf("http://%s/?token=%s", ln.Addr().String(), s.token)
	go func() {
		srv := &http.Server{Handler: s.authMiddleware(s.mux)}
		_ = srv.Serve(ln)
	}()
	// Auto-shutdown watchdog for windowless GUI mode (-H=windowsgui):
	// Once the UI connects, if the window is closed (no heartbeat for 20s) AND no background jobs are active, exit cleanly!
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
	return ln, url, nil
}

// LaunchDesktopWindow opens the local YTCloud UI in native app mode (Edge/Chrome --app) or default browser.
func LaunchDesktopWindow(appURL string) {
	if runtime.GOOS == "windows" {
		// Prefer Edge/Chrome --app mode so it opens as a clean standalone desktop app window!
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
					if strings.Contains(appURL, "127.0.0.1") {
						go sysutil.MaximizeWindowByTitle("BrightVault —", 6*time.Second)
					}
					return
				}
			}
		}
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", appURL).Start()
		if strings.Contains(appURL, "127.0.0.1") {
			go sysutil.MaximizeWindowByTitle("BrightVault —", 6*time.Second)
		}
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
		// Initial load via ?token=... sets an HttpOnly SameSite=Strict session cookie
		if qTok := r.URL.Query().Get("token"); qTok != "" && qTok == s.token {
			http.SetCookie(w, &http.Cookie{
				Name:     "ytcloud_session",
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
		// Protect all /api/* routes strictly
		if strings.HasPrefix(r.URL.Path, "/api/") {
			ok := r.Header.Get("X-YTCloud-Token") == s.token
			if !ok {
				if c, err := r.Cookie("ytcloud_session"); err == nil && c.Value == s.token {
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

	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/update/status", s.handleUpdateStatus)
	s.mux.HandleFunc("/api/update/check", s.handleUpdateCheck)
	s.mux.HandleFunc("/api/update/apply", s.handleUpdateApply)
	s.mux.HandleFunc("/api/heartbeat", s.handleHeartbeat)
	s.mux.HandleFunc("/api/events", s.handleEvents)
	s.mux.HandleFunc("/api/shutdown", s.handleShutdown)
	s.mux.HandleFunc("/api/system/pick-folder", s.handlePickFolder)
	s.mux.HandleFunc("/api/system/pick-files", s.handlePickFiles)
	s.mux.HandleFunc("/api/system/pick-media", s.handlePickMedia)
	s.mux.HandleFunc("/api/system/path-stats", s.handlePathStats)
	s.mux.HandleFunc("/api/media/probe-duration", s.handleProbeMediaDuration)
	s.mux.HandleFunc("/api/preview/gif", s.handlePreviewGIF)
	s.mux.HandleFunc("/api/preview/audio", s.handlePreviewAudio)

	s.mux.HandleFunc("/api/vault/create", s.handleVaultCreate)
	s.mux.HandleFunc("/api/vault/unlock", s.handleVaultUnlock)
	s.mux.HandleFunc("/api/vault/unlock-remembered", s.handleVaultUnlockRemembered)
	s.mux.HandleFunc("/api/vault/forget-password", s.handleVaultForgetPassword)
	s.mux.HandleFunc("/api/vault/lock", s.handleVaultLock)
	s.mux.HandleFunc("/api/vault/password", s.handleVaultPassword)
	s.mux.HandleFunc("/api/vault/generate-keyfile", s.handleGenerateKeyfile)
	s.mux.HandleFunc("/api/vault/state", s.handleVaultState)
	s.mux.HandleFunc("/api/settings", s.handleSettings)
	s.mux.HandleFunc("/api/performance", s.handlePerformance)

	s.mux.HandleFunc("/api/folders", s.handleFolders)
	s.mux.HandleFunc("/api/folders/reorder", s.handleFoldersReorder)
	s.mux.HandleFunc("/api/folders/", s.handleFolderItem)
	s.mux.HandleFunc("/api/collections", s.handleCollections)
	s.mux.HandleFunc("/api/collections/", s.handleCollectionItem)

	s.mux.HandleFunc("/api/files/search", s.handleFilesSearch)
	s.mux.HandleFunc("/api/files/batch-metadata", s.handleBatchMetadata)
	s.mux.HandleFunc("/api/files/delete-batch", s.handleFilesDeleteBatch)
	s.mux.HandleFunc("/api/files/free-local", s.handleFreeLocal)
	s.mux.HandleFunc("/api/files/", s.handleFileItem)

	s.mux.HandleFunc("/api/upload/path", s.handleUploadPath)
	s.mux.HandleFunc("/api/upload/paths", s.handleUploadPaths)
	s.mux.HandleFunc("/api/upload/files", s.handleUploadFiles)
	s.mux.HandleFunc("/api/containers/extract", s.handleExtract)
	s.mux.HandleFunc("/api/containers/upload-all", s.handleContainersUploadAll)
	s.mux.HandleFunc("/api/containers/", s.handleContainerAction)
	s.mux.HandleFunc("/api/peek", s.handlePeek)
	s.mux.HandleFunc("/api/peek/extract", s.handlePeekExtract)
	s.mux.HandleFunc("/api/public/peek", s.handlePublicPeek)
	s.mux.HandleFunc("/api/public/extract", s.handlePublicExtract)
	s.mux.HandleFunc("/api/public/folder-inspect", s.handlePublicFolderInspect)
	s.mux.HandleFunc("/api/public/folder-extract", s.handlePublicFolderExtract)
	s.mux.HandleFunc("/api/downloads/", s.handleDownloadAction)
	s.mux.HandleFunc("/api/sync/analyze", s.handleSyncAnalyze)
	s.mux.HandleFunc("/api/sync/run", s.handleSyncRun)

	s.mux.HandleFunc("/api/catalog/backup", s.handleCatalogBackup)
	s.mux.HandleFunc("/api/catalog/restore", s.handleCatalogRestore)
	s.mux.HandleFunc("/api/catalog/export-file", s.handleCatalogExportFile)
	s.mux.HandleFunc("/api/catalog/discover-yt", s.handleCatalogDiscoverYT)

	s.mux.HandleFunc("/api/youtube/oauth/start", s.handleOAuthStart)
	s.mux.HandleFunc("/api/youtube/oauth/reconnect", s.handleOAuthReconnect)
	s.mux.HandleFunc("/api/tools/install", s.handleInstallTools)
	s.mux.HandleFunc("/api/calibrate", s.handleCalibrate)
	s.mux.HandleFunc("/api/jobs", s.handleJobs)
}

func (s *Server) createJob(kind, title string) *Job {
	id, _ := crypto.RandomHex(6)
	j := &Job{
		ID:        id,
		Kind:      kind,
		Title:     title,
		Status:    "running",
		Progress:  0,
		StartedAt: time.Now().Format("15:04:05"),
	}
	s.jobsMu.Lock()
	s.jobs = append([]*Job{j}, s.jobs...)
	if len(s.jobs) > 40 {
		s.jobs = s.jobs[:40]
	}
	s.jobsMu.Unlock()
	return j
}

func (s *Server) updateJob(j *Job, progress int, detail string) {
	s.jobsMu.Lock()
	j.Progress = progress
	j.Detail = detail
	s.jobsMu.Unlock()
}

func (s *Server) updateJobETA(j *Job, progress int, detail, etaText, speedText string) {
	s.jobsMu.Lock()
	j.Progress = progress
	j.Detail = detail
	j.ETAText = etaText
	j.SpeedText = speedText
	s.jobsMu.Unlock()
}

func fmtETA(d time.Duration) string {
	if d <= 0 {
		return "calculando..."
	}
	secs := int(d.Seconds())
	if secs < 1 {
		return "< 1s"
	}
	if secs < 60 {
		return fmt.Sprintf("~%ds restantes", secs)
	}
	mins := secs / 60
	rem := secs % 60
	if mins < 60 {
		return fmt.Sprintf("~%dm %02ds restantes", mins, rem)
	}
	hrs := mins / 60
	mins = mins % 60
	return fmt.Sprintf("~%dh %02dm restantes", hrs, mins)
}

func (s *Server) finishJob(j *Job, result any, err error) {
	s.jobsMu.Lock()
	j.EndedAt = time.Now().Format("15:04:05")
	j.ETAText = ""
	j.SpeedText = ""
	if err != nil {
		j.Status = "error"
		j.Error = err.Error()
	} else {
		j.Status = "done"
		j.Progress = 100
		j.Result = result
	}
	s.jobsMu.Unlock()
	runtime.GC()
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

func (s *Server) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	s.activeDialogs.Add(1)
	s.lastHeartbeat.Store(time.Now().Unix())
	defer func() {
		s.lastHeartbeat.Store(time.Now().Unix())
		s.activeDialogs.Add(-1)
	}()
	p, err := sysutil.PickFolderDialog("Selecione a pasta para enviar ao BrightVault")
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if p == "" {
		writeJSON(w, map[string]any{"cancelled": true})
		return
	}
	inputs, err := packer.CollectInputsFromPath(p)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	var totalBytes int64
	for _, inp := range inputs {
		if fi, err := os.Stat(inp.LocalPath); err == nil {
			totalBytes += fi.Size()
		}
	}
	writeJSON(w, map[string]any{
		"path":        p,
		"file_count":  len(inputs),
		"total_bytes": totalBytes,
	})
}

func (s *Server) handlePickFiles(w http.ResponseWriter, r *http.Request) {
	s.activeDialogs.Add(1)
	s.lastHeartbeat.Store(time.Now().Unix())
	defer func() {
		s.lastHeartbeat.Store(time.Now().Unix())
		s.activeDialogs.Add(-1)
	}()
	paths, err := sysutil.PickFilesDialog("Selecione os arquivos para enviar ao BrightVault")
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if len(paths) == 0 {
		writeJSON(w, map[string]any{"cancelled": true})
		return
	}
	var totalBytes int64
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			totalBytes += fi.Size()
		}
	}
	writeJSON(w, map[string]any{
		"paths":       paths,
		"file_count":  len(paths),
		"total_bytes": totalBytes,
	})
}

func (s *Server) handlePickMedia(w http.ResponseWriter, r *http.Request) {
	s.activeDialogs.Add(1)
	s.lastHeartbeat.Store(time.Now().Unix())
	defer func() {
		s.lastHeartbeat.Store(time.Now().Unix())
		s.activeDialogs.Add(-1)
	}()
	kind := r.URL.Query().Get("kind")
	title := "Selecione um arquivo de vídeo (.mp4, .mkv, .webm, .mov)"
	if kind == "audio" {
		title = "Selecione um arquivo de áudio (.mp3, .wav, .m4a, .ogg, .flac)"
	}
	paths, err := sysutil.PickFilesDialog(title)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if len(paths) == 0 {
		writeJSON(w, map[string]any{"cancelled": true})
		return
	}
	writeJSON(w, map[string]any{
		"path": paths[0],
		"name": filepath.Base(paths[0]),
	})
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
		writeJSON(w, map[string]any{"file_count": 0, "total_bytes": 0})
		return
	}
	inputs, err := packer.CollectInputsFromPath(p)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	var totalBytes int64
	for _, inp := range inputs {
		if fi, err := os.Stat(inp.LocalPath); err == nil {
			totalBytes += fi.Size()
		}
	}
	writeJSON(w, map[string]any{
		"file_count":  len(inputs),
		"total_bytes": totalBytes,
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
		ytOpts := s.ytDlpOpts()
		runProbe := func(extra []string) (string, string, error) {
			args := []string{"--no-playlist", "--no-warnings", "--print", "%(duration)s|%(title)s"}
			if tools.JSRuntime != "" {
				args = append(args, "--js-runtimes", tools.JSRuntime)
			}
			if ytOpts.CookiesBrowser != "" {
				args = append(args, "--cookies-from-browser", ytOpts.CookiesBrowser)
			} else if ytOpts.CookiesFile != "" {
				args = append(args, "--cookies", ytOpts.CookiesFile)
			}
			args = append(args, extra...)
			args = append(args, src)
			cmd := exec.Command(tools.YtDlpPath, args...)
			sysutil.ApplySubprocessAttrs(cmd, true)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			return string(out), stderr.String(), err
		}
		outStr, errStr, err := runProbe(nil)
		if err != nil {
			// Automatic fallback to android,web player client when YouTube SABR/visionos fails!
			outStr, errStr, err = runProbe([]string{"--extractor-args", "youtube:player_client=android,web"})
		}
		if err != nil {
			writeError(w, 400, fmt.Sprintf("não foi possível consultar a duração do link: %s", strings.TrimSpace(errStr)))
			return
		}
		line := strings.TrimSpace(strings.Split(strings.TrimSpace(outStr), "\n")[0])
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

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.clientConnected.Store(true)
	s.lastHeartbeat.Store(time.Now().Unix())
	tools := media.DetectTools()
	resList := []codec.Resolution{
		codec.Resolutions["2k"],
		codec.Resolutions["4k"],
		codec.Resolutions["1080p"],
		codec.Resolutions["720p"],
	}
	vaultUsesKF := false
	vaultKFHint := ""
	if s.store.IsUnlocked() {
		vaultUsesKF, vaultKFHint = s.store.KeyfileInfo()
	} else if s.store.Exists() {
		if hdr, err := s.store.InspectHeader(); err == nil && hdr != nil && hdr.KeyBundle != nil {
			vaultUsesKF = hdr.KeyBundle.UsesKeyfile
			vaultKFHint = hdr.KeyBundle.KeyfileHint
		}
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
	resp := map[string]any{
		"version":                   updater.AppVersion,
		"update":                    s.updater.Status(),
		"vault_exists":              s.store.Exists(),
		"vault_unlocked":            s.store.IsUnlocked(),
		"vault_uses_keyfile":        vaultUsesKF,
		"vault_keyfile_hint":        vaultKFHint,
		"vault_path":                s.store.Path(),
		"has_remembered_password":   s.store.HasRememberedPassword(),
		"explicitly_locked":         s.explicitlyLocked.Load(),
		"system_info": map[string]any{
			"cpu_cores":    runtime.NumCPU(),
			"total_ram_mb": sysutil.TotalSystemRAMMB(),
		},
		"tools":                     tools,
		"profiles":                  profList,
		"resolutions":               resList,
		"art_themes":                codec.ListArtThemes(),
		"frame_capacities":          frameCaps,
		"frame_capacities_detailed": frameCapsDetailed,
	}
	if s.store.IsUnlocked() {
		_ = s.store.Read(func(d *vault.VaultData) error {
			var totalBytes int64
			for _, f := range d.Files {
				totalBytes += f.SizeBytes
			}
			resp["vault_name"] = d.VaultName
			resp["vault_id"] = d.VaultID
			resp["stats"] = map[string]any{
				"files":       len(d.Files),
				"folders":     len(d.Folders),
				"collections": len(d.Collections),
				"containers":  len(d.Containers),
				"total_bytes": totalBytes,
			}
			return nil
		})
	}
	writeJSON(w, resp)
}

func readKeyfileOptional(path string) ([]byte, string, error) {
	p := strings.Trim(strings.TrimSpace(path), `"`)
	if p == "" {
		return nil, "", nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, "", fmt.Errorf("não foi possível ler o Arquivo-Chave (%s): %w", p, err)
	}
	if len(b) == 0 {
		return nil, "", errors.New("o Arquivo-Chave selecionado está vazio")
	}
	return b, filepath.Base(p), nil
}

func (s *Server) handleGenerateKeyfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, 405, "use POST")
		return
	}
	s.activeDialogs.Add(1)
	s.lastHeartbeat.Store(time.Now().Unix())
	defer func() {
		s.lastHeartbeat.Store(time.Now().Unix())
		s.activeDialogs.Add(-1)
	}()
	folder, err := sysutil.PickFolderDialog("Escolha onde salvar o Arquivo-Chave (.ytkey) — guarde em local seguro!")
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if folder == "" {
		writeJSON(w, map[string]any{"canceled": true})
		return
	}
	suffix, _ := crypto.RandomHex(3)
	fullPath := filepath.Join(folder, fmt.Sprintf("YTCloud-Chave-%s.ytkey", strings.ToUpper(suffix)))
	if _, err := crypto.GenerateKeyfile(fullPath); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"path": fullPath, "name": filepath.Base(fullPath)})
}

func (s *Server) handleVaultCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VaultName   string `json:"vault_name"`
		Password    string `json:"password"`
		KeyfilePath string `json:"keyfile_path"`
		Remember    bool   `json:"remember"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	kfBytes, kfHint, err := readKeyfileOptional(req.KeyfilePath)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	defer crypto.ZeroBytes(kfBytes)

	if err := s.store.CreateWithKeyfile(req.VaultName, req.Password, kfBytes, kfHint); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if req.Remember {
		_ = s.store.SaveRememberedCreds(req.Password, strings.Trim(strings.TrimSpace(req.KeyfilePath), `"`))
	}
	s.explicitlyLocked.Store(false)
	s.applyVaultResourceLimits()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleVaultUnlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password    string `json:"password"`
		KeyfilePath string `json:"keyfile_path"`
		Remember    bool   `json:"remember"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	kfBytes, _, err := readKeyfileOptional(req.KeyfilePath)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	defer crypto.ZeroBytes(kfBytes)

	if err := s.store.UnlockWithKeyfile(req.Password, kfBytes); err != nil {
		writeError(w, 401, err.Error())
		return
	}
	if req.Remember {
		_ = s.store.SaveRememberedCreds(req.Password, strings.Trim(strings.TrimSpace(req.KeyfilePath), `"`))
	}
	s.explicitlyLocked.Store(false)
	s.applyVaultResourceLimits()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleVaultUnlockRemembered(w http.ResponseWriter, r *http.Request) {
	pass, kfPath, err := s.store.LoadRememberedCreds()
	if err != nil {
		writeError(w, 401, "nenhuma senha salva encontrada")
		return
	}
	var kfBytes []byte
	if kfPath != "" {
		kfBytes, _, err = readKeyfileOptional(kfPath)
		if err != nil {
			writeError(w, 401, "Arquivo-Chave salvo não encontrado neste caminho — selecione manualmente")
			return
		}
		defer crypto.ZeroBytes(kfBytes)
	}
	if err := s.store.UnlockWithKeyfile(pass, kfBytes); err != nil {
		_ = s.store.ClearRememberedPassword()
		writeError(w, 401, "credenciais salvas inválidas — digite manualmente")
		return
	}
	s.explicitlyLocked.Store(false)
	s.applyVaultResourceLimits()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleVaultForgetPassword(w http.ResponseWriter, r *http.Request) {
	_ = s.store.ClearRememberedPassword()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleVaultLock(w http.ResponseWriter, r *http.Request) {
	s.explicitlyLocked.Store(true)
	s.store.Lock()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleVaultPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword    string `json:"current_password"`
		CurrentKeyfilePath string `json:"current_keyfile_path"`
		NewPassword        string `json:"new_password"`
		NewKeyfilePath     string `json:"new_keyfile_path"`
		RemoveKeyfile      bool   `json:"remove_keyfile"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	curKF, _, err := readKeyfileOptional(req.CurrentKeyfilePath)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	defer crypto.ZeroBytes(curKF)

	var newKF []byte
	var newHint string
	newKFPath := strings.Trim(strings.TrimSpace(req.NewKeyfilePath), `"`)
	if !req.RemoveKeyfile {
		if newKFPath != "" {
			newKF, newHint, err = readKeyfileOptional(newKFPath)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
			defer crypto.ZeroBytes(newKF)
		} else if len(curKF) > 0 {
			// Keep existing keyfile when only changing password
			newKF = curKF
			_, newHint = s.store.KeyfileInfo()
			newKFPath = strings.Trim(strings.TrimSpace(req.CurrentKeyfilePath), `"`)
		}
	} else {
		newKFPath = ""
	}

	if err := s.store.ChangeAuth(req.CurrentPassword, curKF, req.NewPassword, newKF, newHint, newKFPath); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleVaultState(w http.ResponseWriter, r *http.Request) {
	var out map[string]any
	err := s.store.Read(func(d *vault.VaultData) error {
		folders := make([]*vault.Folder, 0, len(d.Folders))
		for _, f := range d.Folders {
			cp := *f
			cp.FolderKeyHex = "" // kept private inside vault; only shown in Folder Properties
			folders = append(folders, &cp)
		}
		sort.Slice(folders, func(i, j int) bool {
			if folders[i].ID == "root" {
				return true
			}
			if folders[j].ID == "root" {
				return false
			}
			if folders[i].SortOrder != folders[j].SortOrder {
				if folders[i].SortOrder == 0 {
					return false
				}
				if folders[j].SortOrder == 0 {
					return true
				}
				return folders[i].SortOrder < folders[j].SortOrder
			}
			return strings.ToLower(folders[i].Name) < strings.ToLower(folders[j].Name)
		})

		cols := make([]*vault.Collection, 0, len(d.Collections))
		for _, c := range d.Collections {
			cols = append(cols, c)
		}
		sort.Slice(cols, func(i, j int) bool { return strings.ToLower(cols[i].Name) < strings.ToLower(cols[j].Name) })

		conts := make([]*vault.VideoContainer, 0, len(d.Containers))
		for _, c := range d.Containers {
			cp := *c
			cp.FolderKeyHex = "" // never leak container folder key in normal list
			conts = append(conts, &cp)
		}
		sort.Slice(conts, func(i, j int) bool { return conts[i].CreatedAt > conts[j].CreatedAt })

		tagSet := map[string]int{}
		yearSet := map[string]int{}
		for _, f := range d.Files {
			for _, t := range f.Tags {
				tagSet[t]++
			}
			if len(f.EffectiveDate) >= 4 {
				yearSet[f.EffectiveDate[:4]]++
			}
		}
		tags := make([]string, 0, len(tagSet))
		for t := range tagSet {
			tags = append(tags, t)
		}
		sort.Strings(tags)

		years := make([]string, 0, len(yearSet))
		for y := range yearSet {
			years = append(years, y)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(years)))

		ytConnected := d.YouTubeAuth != nil && d.YouTubeAuth.RefreshToken != ""
		ytChannel := ""
		if d.YouTubeAuth != nil {
			ytChannel = d.YouTubeAuth.ChannelTitle
		}

		// Mask client secret in response
		safeSettings := d.Settings
		if safeSettings.YouTubeClientSecret != "" {
			safeSettings.YouTubeClientSecret = "••••••••"
		}

		syncGroups := make([]*vault.SyncGroup, 0, len(d.SyncGroups))
		for _, sg := range d.SyncGroups {
			syncGroups = append(syncGroups, sg)
		}
		sort.Slice(syncGroups, func(i, j int) bool { return syncGroups[i].LastSyncedAt > syncGroups[j].LastSyncedAt })

		pendingDLs := make([]*vault.PendingDownload, 0, len(d.PendingDownloads))
		for _, pd := range d.PendingDownloads {
			pendingDLs = append(pendingDLs, pd)
		}
		sort.Slice(pendingDLs, func(i, j int) bool { return pendingDLs[i].CreatedAt > pendingDLs[j].CreatedAt })

		profileStatus := "none"
		if d.LastCatalogVID != "" {
			if d.DataRevision > d.SyncedRevision {
				profileStatus = "outdated"
			} else {
				profileStatus = "synced"
			}
		}

		out = map[string]any{
			"vault_id":          d.VaultID,
			"vault_name":        d.VaultName,
			"catalog_seq":       d.CatalogSeq,
			"data_revision":     d.DataRevision,
			"synced_revision":   d.SyncedRevision,
			"profile_status":    profileStatus,
			"last_catalog_vid":  d.LastCatalogVID,
			"prev_catalog_vid":  d.PrevCatalogVID,
			"settings":          safeSettings,
			"folders":           folders,
			"collections":       cols,
			"containers":        conts,
			"sync_groups":       syncGroups,
			"pending_downloads": pendingDLs,
			"tags":              tags,
			"years":             years,
			"youtube_connected": ytConnected,
			"youtube_channel":   ytChannel,
		}
		return nil
	})
	if err != nil {
		writeError(w, 401, err.Error())
		return
	}
	writeJSON(w, out)
}

func (s *Server) ytDlpOpts() youtube.YtDlpOptions {
	tools := media.DetectTools()
	opts := youtube.YtDlpOptions{
		YtDlpPath:  tools.YtDlpPath,
		FFmpegPath: tools.FFmpegPath,
		JSRuntime:  tools.JSRuntime,
	}
	if s.store.IsUnlocked() {
		_ = s.store.Read(func(d *vault.VaultData) error {
			opts.CookiesBrowser = d.Settings.YtDlpCookiesBrowser
			opts.CookiesFile = d.Settings.YtDlpCookiesFile
			return nil
		})
	}
	return opts
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req vault.VaultSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	err := s.store.Update(func(d *vault.VaultData) error {
		if req.DefaultResolution != "" {
			d.Settings.DefaultResolution = req.DefaultResolution
		}
		if req.DefaultProfile != "" {
			d.Settings.DefaultProfile = req.DefaultProfile
		}
		if req.DefaultVideoCodec != "" {
			d.Settings.DefaultVideoCodec = req.DefaultVideoCodec
		}
		if req.DefaultFPS > 0 {
			d.Settings.DefaultFPS = req.DefaultFPS
		}
		if req.MaxRAMMB >= 256 {
			d.Settings.MaxRAMMB = req.MaxRAMMB
		}
		if req.CPUThreads >= 1 {
			d.Settings.CPUThreads = req.CPUThreads
		}
		if req.MaxCPUPercent >= 10 && req.MaxCPUPercent <= 100 {
			d.Settings.MaxCPUPercent = req.MaxCPUPercent
		}
		if req.MaxRAMPercent >= 10 && req.MaxRAMPercent <= 90 {
			d.Settings.MaxRAMPercent = req.MaxRAMPercent
		}
		if req.PerfPreset != "" {
			d.Settings.PerfPreset = req.PerfPreset
		}
		d.Settings.BackgroundMode = req.BackgroundMode
		if req.CleanupPolicy != "" {
			d.Settings.CleanupPolicy = req.CleanupPolicy
		}
		d.Settings.CatalogKeepPrevious = req.CatalogKeepPrevious
		d.Settings.CatalogIncludeThumbs = req.CatalogIncludeThumbs
		d.Settings.OptimizePNGLossless = req.OptimizePNGLossless
		if req.MaxDeltaVideos != nil {
			v := *req.MaxDeltaVideos
			if v < 0 {
				v = 0
			} else if v > 99 {
				v = 99
			}
			d.Settings.MaxDeltaVideos = &v
			for _, sg := range d.SyncGroups {
				sg.MaxDeltas = v
			}
		}
		if p := normalizePrivacy(req.DefaultPrivacy); p != "" {
			d.Settings.DefaultPrivacy = p
		}
		if req.Theme != "" {
			d.Settings.Theme = strings.TrimSpace(req.Theme)
		}
		if req.Language != "" {
			d.Settings.Language = strings.TrimSpace(req.Language)
		}
		d.Settings.YtDlpCookiesBrowser = strings.TrimSpace(req.YtDlpCookiesBrowser)
		d.Settings.YtDlpCookiesFile = strings.Trim(strings.TrimSpace(req.YtDlpCookiesFile), `"`)
		if req.YouTubeClientID != "" {
			d.Settings.YouTubeClientID = strings.TrimSpace(req.YouTubeClientID)
		}
		if req.YouTubeClientSecret != "" && !strings.HasPrefix(req.YouTubeClientSecret, "••") {
			d.Settings.YouTubeClientSecret = strings.TrimSpace(req.YouTubeClientSecret)
		}
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	s.applyVaultResourceLimits()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handlePerformance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		PerfPreset          string `json:"perf_preset"`
		MaxCPUPercent       int    `json:"max_cpu_percent"`
		MaxRAMPercent       int    `json:"max_ram_percent"`
		BackgroundMode      *bool  `json:"background_mode"`
		OptimizePNGLossless *bool  `json:"optimize_png_lossless"`
		CleanupPolicy       string `json:"cleanup_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	cores := runtime.NumCPU()
	if cores < 1 {
		cores = 1
	}
	totalRAM := sysutil.TotalSystemRAMMB()

	cpuPct := req.MaxCPUPercent
	ramPct := req.MaxRAMPercent
	if cpuPct < 10 {
		cpuPct = 10
	} else if cpuPct > 100 {
		cpuPct = 100
	}
	if ramPct < 10 {
		ramPct = 10
	} else if ramPct > 85 {
		ramPct = 85
	}
	threads := (cores*cpuPct + 50) / 100
	if threads < 1 {
		threads = 1
	}
	if threads > cores {
		threads = cores
	}
	maxRAM := (totalRAM * ramPct) / 100
	if maxRAM < 256 {
		maxRAM = 256
	}

	err := s.store.Update(func(d *vault.VaultData) error {
		d.Settings.PerfPreset = req.PerfPreset
		d.Settings.MaxCPUPercent = cpuPct
		d.Settings.MaxRAMPercent = ramPct
		d.Settings.CPUThreads = threads
		d.Settings.MaxRAMMB = maxRAM
		if req.BackgroundMode != nil {
			d.Settings.BackgroundMode = *req.BackgroundMode
		}
		if req.OptimizePNGLossless != nil {
			d.Settings.OptimizePNGLossless = *req.OptimizePNGLossless
		}
		if req.CleanupPolicy != "" {
			d.Settings.CleanupPolicy = req.CleanupPolicy
		}
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	s.applyVaultResourceLimits()
	writeJSON(w, map[string]any{
		"ok":              true,
		"cpu_threads":     threads,
		"max_ram_mb":      maxRAM,
		"max_cpu_percent": cpuPct,
		"max_ram_percent": ramPct,
		"perf_preset":     req.PerfPreset,
	})
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		ParentID string `json:"parent_id"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeError(w, 400, "nome da pasta obrigatório")
		return
	}
	if req.ParentID == "" {
		req.ParentID = "root"
	}
	var created *vault.Folder
	err := s.store.Update(func(d *vault.VaultData) error {
		id, _ := crypto.RandomHex(6)
		fk, _ := crypto.RandomHex(32)
		maxOrd := 0
		for _, existing := range d.Folders {
			if existing.ParentID == req.ParentID && existing.SortOrder > maxOrd {
				maxOrd = existing.SortOrder
			}
		}
		created = &vault.Folder{
			ID:           id,
			ParentID:     req.ParentID,
			Name:         strings.TrimSpace(req.Name),
			SortOrder:    maxOrd + 1,
			FolderKeyHex: fk,
			CreatedAt:    time.Now().Format(time.RFC3339),
		}
		d.Folders[id] = created
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	safeResp := *created
	safeResp.FolderKeyHex = ""
	writeJSON(w, &safeResp)
}

func (s *Server) handleFolderItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/folders/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if id == "" {
		writeError(w, 400, "id da pasta obrigatório")
		return
	}
	if action == "props" {
		s.handleFolderProps(w, r, id)
		return
	}
	if action == "share-code" {
		s.handleFolderShareCode(w, r, id)
		return
	}
	if action == "wrap-parent" {
		s.handleFolderWrapParent(w, r, id)
		return
	}
	if id == "root" {
		writeError(w, 400, "pasta raiz não pode ser removida")
		return
	}
	switch r.Method {
	case "PATCH":
		var req struct {
			Name     string `json:"name"`
			ParentID string `json:"parent_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "JSON inválido")
			return
		}
		err := s.store.Update(func(d *vault.VaultData) error {
			f, ok := d.Folders[id]
			if !ok {
				return errors.New("pasta não encontrada")
			}
			if strings.TrimSpace(req.Name) != "" {
				f.Name = strings.TrimSpace(req.Name)
			}
			if req.ParentID != "" && req.ParentID != id {
				f.ParentID = req.ParentID
			}
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "DELETE":
		mode := r.URL.Query().Get("mode") // "recursive" (delete folder + all subfolders + files) or "move_up"
		deleteYT := r.URL.Query().Get("delete_yt") == "true"
		var removedFiles, removedFolders int
		var orphanedYT []string

		err := s.store.Update(func(d *vault.VaultData) error {
			f, ok := d.Folders[id]
			if !ok {
				return errors.New("pasta não encontrada")
			}
			if mode == "move_up" {
				parent := f.ParentID
				if parent == "" {
					parent = "root"
				}
				for _, sub := range d.Folders {
					if sub.ParentID == id {
						sub.ParentID = parent
					}
				}
				for _, fe := range d.Files {
					if fe.FolderID == id {
						fe.FolderID = parent
					}
				}
				delete(d.Folders, id)
				removedFolders = 1
				return nil
			}

			// Default: recursive delete (folder + all nested subfolders + all files inside them)
			toDeleteFolders := map[string]bool{id: true}
			changed := true
			for changed {
				changed = false
				for _, sub := range d.Folders {
					if toDeleteFolders[sub.ParentID] && !toDeleteFolders[sub.ID] {
						toDeleteFolders[sub.ID] = true
						changed = true
					}
				}
			}
			affectedConts := map[string]bool{}
			for fid, fe := range d.Files {
				if toDeleteFolders[fe.FolderID] {
					if fe.ContainerID != "" {
						affectedConts[fe.ContainerID] = true
					}
					delete(d.Files, fid)
					removedFiles++
				}
			}
			for folID := range toDeleteFolders {
				delete(d.Folders, folID)
				removedFolders++
			}
			orphanedYT = cleanupEmptyContainersLocked(d, affectedConts)
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		ytDeleted := 0
		if deleteYT && len(orphanedYT) > 0 {
			if tok, err := s.ensureYouTubeAccessToken(); err == nil && tok != "" {
				for _, vid := range orphanedYT {
					if youtube.DeleteVideoAPI(tok, vid) == nil {
						ytDeleted++
					}
				}
			}
		}
		writeJSON(w, map[string]any{
			"ok":              true,
			"removed_files":   removedFiles,
			"removed_folders": removedFolders,
			"yt_deleted":      ytDeleted,
		})
	}
}

func cleanupEmptyContainersLocked(d *vault.VaultData, candidateIDs map[string]bool) []string {
	remaining := map[string]int{}
	for _, fe := range d.Files {
		remaining[fe.ContainerID]++
	}
	var orphanedYT []string
	for cid := range candidateIDs {
		c, ok := d.Containers[cid]
		if !ok {
			continue
		}
		if remaining[cid] == 0 {
			if c.LocalVideoPath != "" && !c.LocalDeleted {
				_ = os.Remove(c.LocalVideoPath)
			}
			if c.YouTubeVideoID != "" {
				orphanedYT = append(orphanedYT, c.YouTubeVideoID)
			}
			delete(d.Containers, cid)
		} else {
			c.FileCount = remaining[cid]
		}
	}
	return orphanedYT
}

func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Color       string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeError(w, 400, "nome da coleção obrigatório")
		return
	}
	if req.Color == "" {
		req.Color = "#38bdf8"
	}
	var created *vault.Collection
	err := s.store.Update(func(d *vault.VaultData) error {
		id, _ := crypto.RandomHex(6)
		created = &vault.Collection{
			ID:          id,
			Name:        strings.TrimSpace(req.Name),
			Description: strings.TrimSpace(req.Description),
			Color:       req.Color,
			CreatedAt:   time.Now().Format(time.RFC3339),
		}
		d.Collections[id] = created
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, created)
}

func (s *Server) handleCollectionItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/collections/")
	if r.Method == "DELETE" {
		err := s.store.Update(func(d *vault.VaultData) error {
			delete(d.Collections, id)
			for _, f := range d.Files {
				filtered := f.CollectionIDs[:0]
				for _, cid := range f.CollectionIDs {
					if cid != id {
						filtered = append(filtered, cid)
					}
				}
				f.CollectionIDs = filtered
			}
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}
}

func (s *Server) handleFilesSearch(w http.ResponseWriter, r *http.Request) {
	var q vault.SearchQuery
	_ = json.NewDecoder(r.Body).Decode(&q)
	res, err := s.store.SearchFiles(q)
	if err != nil {
		writeError(w, 401, err.Error())
		return
	}
	if res == nil {
		res = []*vault.FileEntry{}
	}
	writeJSON(w, map[string]any{"files": res, "count": len(res)})
}

func (s *Server) handleFileItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/files/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id := parts[0]
	if len(parts) > 1 && parts[1] == "open" {
		s.handleFileOpen(w, r, id)
		return
	}
	switch r.Method {
	case "PATCH":
		var req struct {
			DisplayName   *string  `json:"display_name"`
			EffectiveDate *string  `json:"effective_date"`
			DatePrecision *string  `json:"date_precision"`
			LocationLabel *string  `json:"location_label"`
			Notes         *string  `json:"notes"`
			FolderID      *string  `json:"folder_id"`
			Tags          []string `json:"tags"`
			CollectionIDs []string `json:"collection_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "JSON inválido")
			return
		}
		var updated *vault.FileEntry
		err := s.store.Update(func(d *vault.VaultData) error {
			f, ok := d.Files[id]
			if !ok {
				return errors.New("arquivo não encontrado")
			}
			if req.DisplayName != nil && strings.TrimSpace(*req.DisplayName) != "" {
				f.DisplayName = strings.TrimSpace(*req.DisplayName)
			}
			if req.EffectiveDate != nil {
				f.EffectiveDate = strings.TrimSpace(*req.EffectiveDate)
			}
			if req.DatePrecision != nil {
				f.DatePrecision = strings.TrimSpace(*req.DatePrecision)
			}
			if req.LocationLabel != nil {
				f.LocationLabel = strings.TrimSpace(*req.LocationLabel)
			}
			if req.Notes != nil {
				f.Notes = *req.Notes
			}
			if req.FolderID != nil {
				if _, exists := d.Folders[*req.FolderID]; exists {
					f.FolderID = *req.FolderID
				}
			}
			if req.Tags != nil {
				clean := make([]string, 0, len(req.Tags))
				for _, t := range req.Tags {
					t = strings.TrimSpace(t)
					if t != "" {
						clean = append(clean, t)
					}
				}
				f.Tags = clean
			}
			if req.CollectionIDs != nil {
				f.CollectionIDs = req.CollectionIDs
			}
			updated = f
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, updated)
	case "DELETE":
		deleteYT := r.URL.Query().Get("delete_yt") == "true"
		var orphanedYT []string
		err := s.store.Update(func(d *vault.VaultData) error {
			fe, ok := d.Files[id]
			if !ok {
				return errors.New("arquivo não encontrado")
			}
			cid := fe.ContainerID
			delete(d.Files, id)
			if cid != "" {
				orphanedYT = cleanupEmptyContainersLocked(d, map[string]bool{cid: true})
			}
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if deleteYT && len(orphanedYT) > 0 {
			if tok, err := s.ensureYouTubeAccessToken(); err == nil && tok != "" {
				for _, vid := range orphanedYT {
					_ = youtube.DeleteVideoAPI(tok, vid)
				}
			}
		}
		writeJSON(w, map[string]any{"ok": true})
	}
}

func (s *Server) handleFilesDeleteBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FileIDs     []string `json:"file_ids"`
		DeleteEmpty bool     `json:"delete_empty_containers"`
		DeleteYT    bool     `json:"delete_yt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.FileIDs) == 0 {
		writeError(w, 400, "nenhum arquivo selecionado para exclusão")
		return
	}
	removed := 0
	var orphanedYT []string
	err := s.store.Update(func(d *vault.VaultData) error {
		affected := map[string]bool{}
		for _, fid := range req.FileIDs {
			if fe, ok := d.Files[fid]; ok {
				if fe.ContainerID != "" {
					affected[fe.ContainerID] = true
				}
				delete(d.Files, fid)
				removed++
			}
		}
		if req.DeleteEmpty {
			orphanedYT = cleanupEmptyContainersLocked(d, affected)
		}
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ytDeleted := 0
	if req.DeleteYT && len(orphanedYT) > 0 {
		if tok, err := s.ensureYouTubeAccessToken(); err == nil && tok != "" {
			for _, vid := range orphanedYT {
				if youtube.DeleteVideoAPI(tok, vid) == nil {
					ytDeleted++
				}
			}
		}
	}
	writeJSON(w, map[string]any{"removed": removed, "yt_deleted": ytDeleted})
}

func (s *Server) handleBatchMetadata(w http.ResponseWriter, r *http.Request) {
	var req vault.BatchMetadataUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	n, err := s.store.ApplyBatchMetadata(req)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"updated": n})
}

func (s *Server) handleFreeLocal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FileIDs []string `json:"file_ids"`
		Confirm bool     `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !req.Confirm {
		writeError(w, 400, "confirmação explícita necessária para liberar espaço local")
		return
	}
	freed := 0
	var freedBytes int64
	err := s.store.Update(func(d *vault.VaultData) error {
		for _, id := range req.FileIDs {
			f, ok := d.Files[id]
			if !ok || f.LocalSourcePath == "" || f.LocalFreed {
				continue
			}
			c, ok := d.Containers[f.ContainerID]
			if !ok || (c.Status != "uploaded" && c.Status != "verified") {
				continue // Safety lock: never free local originals until uploaded/verified on YouTube!
			}
			if fi, err := os.Stat(f.LocalSourcePath); err == nil {
				if os.Remove(f.LocalSourcePath) == nil {
					freed++
					freedBytes += fi.Size()
					f.LocalFreed = true
				}
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"freed_files": freed, "freed_bytes": freedBytes})
}

// --- Upload & Optical Encoding Pipeline ---

type encodeJobOpts struct {
	FolderID        string
	Resolution      string
	Profile         string
	VideoCodec      string
	FPS             int
	AutoUploadYT    bool
	Privacy         string // optional override: "public", "unlisted", "private"
	VideoTitle      string // optional custom YouTube video title
	CleanupDir      string
	ArtTheme        string
	ArtKeyword      string
	MaskIntensity   string // "subtle", "balanced", "strong", "max"
	HideHeaderBar   bool   // seamless hidden header (no B&W top bar)
	HostVideoSource string // local file path or URL (downloaded via yt-dlp)
	AudioMode        string   // "auto", "none", "ambient", "host", "file"
	AudioSource      string   // local audio file path or URL (downloaded via yt-dlp)
	MusicStyle       string   // "ambient", "lofi", "synthwave", "meditation", "nature", "piano", "chiptune"
	MusicMood        string   // "dreamy", "calm", "melancholic", "energetic", "mysterious"
	MusicInstruments []string // active instruments
	MusicPrompt      string   // optional text/phrase shaping melody, solos & dynamics
	MusicSeed        uint64   // optional custom melody seed
	DurationSource   string   // "files" (default), "video", "audio"
	PaddingMode      string   // "auto" (auto-soften + uniform texture), "spread", "start"
	CrossFECMode     string   // "auto", "max" (K=6,M=2), "high" (K=8,M=2), "off"
	PublicPasswords  []string // non-empty => Public Share mode (1..5 layered passwords, zero vault DEK!)
}

func resolveMediaInput(tools media.ToolStatus, ytOpts youtube.YtDlpOptions, rawSource, tempDir string, audioOnly bool) (string, error) {
	src := strings.Trim(strings.TrimSpace(rawSource), `"`)
	if src == "" {
		return "", nil
	}
	lower := strings.ToLower(src)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		if _, err := os.Stat(src); err != nil {
			return "", fmt.Errorf("arquivo de mídia não encontrado: %s", src)
		}
		return src, nil
	}
	if !tools.HasYtDlp {
		return "", errors.New("yt-dlp não encontrado para baixar mídia por link — instale as ferramentas na barra lateral")
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return "", err
	}
	prefix := "host_dl"
	if audioOnly {
		prefix = "audio_dl"
	}
	outTpl := filepath.Join(tempDir, prefix+".%(ext)s")
	runDL := func(extraArgs []string, formatStr string) (string, error) {
		args := []string{"--no-playlist", "--no-warnings"}
		if tools.FFmpegPath != "" {
			args = append(args, "--ffmpeg-location", filepath.Dir(tools.FFmpegPath))
		}
		if tools.JSRuntime != "" {
			args = append(args, "--js-runtimes", tools.JSRuntime)
		}
		if ytOpts.CookiesBrowser != "" {
			args = append(args, "--cookies-from-browser", ytOpts.CookiesBrowser)
		} else if ytOpts.CookiesFile != "" {
			args = append(args, "--cookies", ytOpts.CookiesFile)
		}
		args = append(args, extraArgs...)
		if audioOnly {
			args = append(args, "-f", formatStr, "-x", "--audio-format", "m4a", "-o", outTpl, src)
		} else {
			args = append(args, "-f", formatStr, "--merge-output-format", "mp4", "-o", outTpl, src)
		}
		cmd := exec.Command(tools.YtDlpPath, args...)
		sysutil.ApplySubprocessAttrs(cmd, true)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		return strings.TrimSpace(stderr.String()), err
	}

	fmtPrimary := "bestvideo[height<=1080]+bestaudio/best[height<=1080]/best"
	if audioOnly {
		fmtPrimary = "bestaudio/best"
	}
	errMsg, err := runDL(nil, fmtPrimary)
	if err != nil {
		// Automatic fallback to android,web player client (solves YouTube visionos/SABR "This video is not available")
		errMsg, err = runDL([]string{"--extractor-args", "youtube:player_client=android,web"}, fmtPrimary)
	}
	if err != nil && !audioOnly {
		// Final fallback: direct single-stream best format
		errMsg, err = runDL([]string{"--extractor-args", "youtube:player_client=android,web"}, "best")
	}
	if err != nil {
		return "", fmt.Errorf("erro baixando mídia do link: %v — %s", err, errMsg)
	}
	matches, _ := filepath.Glob(filepath.Join(tempDir, prefix+".*"))
	for _, m := range matches {
		if !strings.HasSuffix(m, ".part") {
			return m, nil
		}
	}
	return "", errors.New("não foi possível localizar o arquivo de mídia baixado pelo yt-dlp")
}

func (s *Server) handleUploadPath(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path            string `json:"path"`
		FolderID        string `json:"folder_id"`
		Resolution      string `json:"resolution"`
		Profile         string `json:"profile"`
		VideoCodec      string `json:"video_codec"`
		FPS             int    `json:"fps"`
		AutoUploadYT    bool   `json:"auto_upload_yt"`
		Privacy         string `json:"privacy"`
		VideoTitle      string `json:"video_title"`
		IncludePreview  bool   `json:"include_preview"`
		ArtTheme        string `json:"art_theme"`
		ArtKeyword      string `json:"art_keyword"`
		MaskIntensity   string `json:"mask_intensity"`
		HideHeaderBar   bool   `json:"hide_header_bar"`
		HostVideoSource string `json:"host_video_source"`
		AudioMode        string   `json:"audio_mode"`
		AudioSource      string   `json:"audio_source"`
		MusicStyle       string   `json:"music_style"`
		MusicMood        string   `json:"music_mood"`
		MusicInstruments []string `json:"music_instruments"`
		MusicPrompt      string   `json:"music_prompt"`
		MusicSeed        uint64   `json:"music_seed"`
		DurationSource   string   `json:"duration_source"`
		PaddingMode      string   `json:"padding_mode"`
		CrossFECMode     string   `json:"cross_fec_mode"`
		PublicPasswords  []string `json:"public_passwords"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		writeError(w, 400, "informe o caminho do arquivo ou pasta")
		return
	}
	inputs, err := packer.CollectInputsFromPath(strings.Trim(strings.TrimSpace(req.Path), `"`))
	if err != nil || len(inputs) == 0 {
		writeError(w, 400, fmt.Sprintf("erro lendo caminho: %v", err))
		return
	}
	job := s.startEncodeJobOpts(inputs, encodeJobOpts{
		FolderID:         req.FolderID,
		Resolution:       req.Resolution,
		Profile:          req.Profile,
		VideoCodec:       req.VideoCodec,
		FPS:              req.FPS,
		AutoUploadYT:     req.AutoUploadYT,
		Privacy:          req.Privacy,
		VideoTitle:       req.VideoTitle,
		ArtTheme:         req.ArtTheme,
		ArtKeyword:       req.ArtKeyword,
		MaskIntensity:    req.MaskIntensity,
		HideHeaderBar:    req.HideHeaderBar,
		HostVideoSource:  req.HostVideoSource,
		AudioMode:        req.AudioMode,
		AudioSource:      req.AudioSource,
		MusicStyle:       req.MusicStyle,
		MusicMood:        req.MusicMood,
		MusicInstruments: req.MusicInstruments,
		MusicPrompt:      req.MusicPrompt,
		MusicSeed:        req.MusicSeed,
		DurationSource:   req.DurationSource,
		PaddingMode:      req.PaddingMode,
		CrossFECMode:     req.CrossFECMode,
		PublicPasswords:  req.PublicPasswords,
	})
	writeJSON(w, job)
}

func (s *Server) handleUploadPaths(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths            []string `json:"paths"`
		FolderID         string   `json:"folder_id"`
		Resolution       string   `json:"resolution"`
		Profile          string   `json:"profile"`
		VideoCodec       string   `json:"video_codec"`
		FPS              int      `json:"fps"`
		AutoUploadYT     bool     `json:"auto_upload_yt"`
		Privacy          string   `json:"privacy"`
		VideoTitle       string   `json:"video_title"`
		ArtTheme         string   `json:"art_theme"`
		ArtKeyword       string   `json:"art_keyword"`
		MaskIntensity    string   `json:"mask_intensity"`
		HideHeaderBar    bool     `json:"hide_header_bar"`
		HostVideoSource  string   `json:"host_video_source"`
		AudioMode        string   `json:"audio_mode"`
		AudioSource      string   `json:"audio_source"`
		MusicStyle       string   `json:"music_style"`
		MusicMood        string   `json:"music_mood"`
		MusicInstruments []string `json:"music_instruments"`
		MusicPrompt      string   `json:"music_prompt"`
		MusicSeed        uint64   `json:"music_seed"`
		DurationSource   string   `json:"duration_source"`
		PaddingMode      string   `json:"padding_mode"`
		CrossFECMode     string   `json:"cross_fec_mode"`
		PublicPasswords  []string `json:"public_passwords"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Paths) == 0 {
		writeError(w, 400, "nenhum arquivo ou pasta selecionado")
		return
	}
	var all []packer.InputFile
	for _, p := range req.Paths {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p == "" {
			continue
		}
		inps, err := packer.CollectInputsFromPath(p)
		if err != nil {
			writeError(w, 400, fmt.Sprintf("erro lendo %s: %v", p, err))
			return
		}
		all = append(all, inps...)
	}
	if len(all) == 0 {
		writeError(w, 400, "nenhum arquivo encontrado nos caminhos informados")
		return
	}
	job := s.startEncodeJobOpts(all, encodeJobOpts{
		FolderID:         req.FolderID,
		Resolution:       req.Resolution,
		Profile:          req.Profile,
		VideoCodec:       req.VideoCodec,
		FPS:              req.FPS,
		AutoUploadYT:     req.AutoUploadYT,
		Privacy:          req.Privacy,
		VideoTitle:       req.VideoTitle,
		ArtTheme:         req.ArtTheme,
		ArtKeyword:       req.ArtKeyword,
		MaskIntensity:    req.MaskIntensity,
		HideHeaderBar:    req.HideHeaderBar,
		HostVideoSource:  req.HostVideoSource,
		AudioMode:        req.AudioMode,
		AudioSource:      req.AudioSource,
		MusicStyle:       req.MusicStyle,
		MusicMood:        req.MusicMood,
		MusicInstruments: req.MusicInstruments,
		MusicPrompt:      req.MusicPrompt,
		MusicSeed:        req.MusicSeed,
		DurationSource:   req.DurationSource,
		PaddingMode:      req.PaddingMode,
		CrossFECMode:     req.CrossFECMode,
		PublicPasswords:  req.PublicPasswords,
	})
	writeJSON(w, job)
}

// handleUploadFiles streams multipart uploads directly to temporary disk via MultipartReader,
// preventing RAM exhaustion and "Failed to fetch" errors on large folders!
func (s *Server) handleUploadFiles(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, 400, fmt.Sprintf("erro abrindo stream de upload: %v", err))
		return
	}
	batchID, _ := crypto.RandomHex(6)
	tmpDir := filepath.Join(s.stagingDir, "incoming", batchID)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		writeError(w, 500, err.Error())
		return
	}

	var (
		folderID         string
		resCode          string
		profCode         string
		videoCodec       string
		fps              int
		autoYT           bool
		privacy          string
		videoTitle       string
		artTheme         string
		artKeyword       string
		maskIntensity    string
		hideHeaderBar    bool
		hostVideoSource  string
		audioMode        string
		audioSource      string
		musicStyle       string
		musicMood        string
		musicInstruments []string
		musicPrompt      string
		musicSeed        uint64
		durationSource   string
		paddingMode      string
		crossFECMode     string
		publicPasswords  []string
		relPaths         []string
		modTimes         []string
		diskPaths        []string
		fileNames        []string
	)

	copyBuf := make([]byte, 64*1024)
	fileIdx := 0

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			writeError(w, 400, fmt.Sprintf("upload interrompido: %v", err))
			return
		}
		formName := part.FormName()
		if formName == "files" {
			dstName := fmt.Sprintf("f_%06d_%s", fileIdx, filepath.Base(part.FileName()))
			dstPath := filepath.Join(tmpDir, dstName)
			outF, err := os.Create(dstPath)
			if err != nil {
				part.Close()
				_ = os.RemoveAll(tmpDir)
				writeError(w, 500, err.Error())
				return
			}
			_, copyErr := io.CopyBuffer(outF, part, copyBuf)
			outF.Close()
			part.Close()
			if copyErr != nil {
				_ = os.RemoveAll(tmpDir)
				writeError(w, 400, fmt.Sprintf("erro salvando arquivo temporário: %v", copyErr))
				return
			}
			diskPaths = append(diskPaths, dstPath)
			fileNames = append(fileNames, part.FileName())
			fileIdx++
		} else {
			valBytes, _ := io.ReadAll(io.LimitReader(part, 1<<20))
			part.Close()
			val := string(valBytes)
			switch formName {
			case "folder_id":
				folderID = val
			case "resolution":
				resCode = val
			case "profile":
				profCode = val
			case "video_codec":
				videoCodec = val
			case "fps":
				fmt.Sscanf(val, "%d", &fps)
			case "auto_upload_yt":
				autoYT = val == "true"
			case "privacy":
				privacy = val
			case "video_title":
				videoTitle = val
			case "art_theme":
				artTheme = val
			case "art_keyword":
				artKeyword = val
			case "mask_intensity":
				maskIntensity = val
			case "hide_header_bar":
				hideHeaderBar = val == "true"
			case "host_video_source":
				hostVideoSource = val
			case "audio_mode":
				audioMode = val
			case "audio_source":
				audioSource = val
			case "music_style":
				musicStyle = val
			case "music_mood":
				musicMood = val
			case "music_instruments":
				for _, p := range strings.Split(val, ",") {
					if p = strings.TrimSpace(p); p != "" {
						musicInstruments = append(musicInstruments, p)
					}
				}
			case "music_prompt":
				musicPrompt = val
			case "music_seed":
				fmt.Sscanf(val, "%d", &musicSeed)
			case "duration_source":
				durationSource = val
			case "padding_mode":
				paddingMode = val
			case "cross_fec_mode":
				crossFECMode = val
			case "public_passwords":
				var pws []string
				if json.Unmarshal([]byte(val), &pws) == nil {
					publicPasswords = pws
				} else if val != "" {
					publicPasswords = append(publicPasswords, val)
				}
			case "rel_paths":
				relPaths = append(relPaths, val)
			case "mod_times":
				modTimes = append(modTimes, val)
			}
		}
	}

	if len(diskPaths) == 0 {
		_ = os.RemoveAll(tmpDir)
		writeError(w, 400, "nenhum arquivo recebido")
		return
	}

	inputs := make([]packer.InputFile, 0, len(diskPaths))
	for i, dp := range diskPaths {
		rel := fileNames[i]
		if i < len(relPaths) && relPaths[i] != "" {
			rel = relPaths[i]
		}
		mt := time.Now().Format("2006-01-02 15:04:05")
		if i < len(modTimes) && modTimes[i] != "" {
			mt = modTimes[i]
		}
		inputs = append(inputs, packer.InputFile{
			RelPath:   rel,
			LocalPath: dp,
			ModTime:   mt,
		})
	}

	job := s.startEncodeJobOpts(inputs, encodeJobOpts{
		FolderID:        folderID,
		Resolution:      resCode,
		Profile:         profCode,
		VideoCodec:      videoCodec,
		FPS:             fps,
		AutoUploadYT:    autoYT,
		Privacy:         privacy,
		VideoTitle:      videoTitle,
		CleanupDir:      tmpDir,
		ArtTheme:        artTheme,
		ArtKeyword:      artKeyword,
		MaskIntensity:   maskIntensity,
		HideHeaderBar:   hideHeaderBar,
		HostVideoSource: hostVideoSource,
		AudioMode:        audioMode,
		AudioSource:      audioSource,
		MusicStyle:       musicStyle,
		MusicMood:        musicMood,
		MusicInstruments: musicInstruments,
		MusicPrompt:      musicPrompt,
		MusicSeed:        musicSeed,
		DurationSource:   durationSource,
		PaddingMode:      paddingMode,
		CrossFECMode:     crossFECMode,
		PublicPasswords:  publicPasswords,
	})
	writeJSON(w, job)
}

func (s *Server) startEncodeJobOpts(inputs []packer.InputFile, req encodeJobOpts) *Job {
	folderID := req.FolderID
	resCode := req.Resolution
	profCode := req.Profile
	videoCodec := req.VideoCodec
	fps := req.FPS
	autoUploadYT := req.AutoUploadYT
	cleanupIncomingDir := req.CleanupDir
	artThemeCode := req.ArtTheme
	artKeyword := req.ArtKeyword
	if folderID == "" {
		folderID = "root"
	}
	job := s.createJob("encode", fmt.Sprintf("Empacotando %d arquivo(s) em vídeo óptico", len(inputs)))

	go func() {
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				_ = os.WriteFile(filepath.Join(media.DefaultAppDir(), "crash.log"),
					[]byte(fmt.Sprintf("[%s] PANIC in encode job: %v\n%s\n", time.Now().Format(time.RFC3339), r, stack)), 0o600)
				s.finishJob(job, nil, fmt.Errorf("erro interno inesperado na codificação: %v", r))
			}
		}()
		if cleanupIncomingDir != "" {
			defer os.RemoveAll(cleanupIncomingDir)
		}
		tools := media.DetectTools()
		if !tools.HasFFmpeg {
			s.finishJob(job, nil, errors.New("FFmpeg não encontrado — clique em 'Instalar FFmpeg & yt-dlp' na barra lateral"))
			return
		}

		var settings vault.VaultSettings
		_ = s.store.Read(func(d *vault.VaultData) error {
			settings = d.Settings
			return nil
		})
		if resCode == "" {
			resCode = settings.DefaultResolution
		}
		if profCode == "" {
			profCode = settings.DefaultProfile
		}
		if videoCodec == "" {
			videoCodec = settings.DefaultVideoCodec
		}
		if fps <= 0 {
			fps = settings.DefaultFPS
		}
		cpuThreads := settings.CPUThreads
		if cpuThreads <= 0 {
			cpuThreads = 2
		}

		packStart := time.Now()
		s.updateJobETA(job, 5, "Comprimindo (Solid zstd Best + Delta-PCM16) e gerando pré-visualizações...", "calculando...", "")
		contUUID, _ := crypto.RandomHex(8)
		shortCode := strings.TrimSpace(req.VideoTitle)
		if shortCode == "" {
			shortCode = "BV-" + contUUID[:8]
		}
		if len(shortCode) > 95 {
			shortCode = shortCode[:95]
		}
		fileBase := sanitizeVideoFileBase(shortCode, "BV-"+contUUID[:8])
		contSalt, _ := crypto.RandomBytes(16)
		contNumericID := binary.LittleEndian.Uint64(contSalt[:8])
		resolvedTheme, mixedSeed := codec.ResolveArtTheme(artThemeCode, artKeyword, contNumericID)
		contNumericID = mixedSeed

		manifest, rawPayload, err := packer.PackFilesWithOptions(inputs, packer.PackOptions{
			ContainerID:          contUUID,
			ProfileCode:          profCode,
			ResolutionCode:       resCode,
			FFmpegPath:           tools.FFmpegPath,
			IncludeMicroPreviews: true,
			OptimizePNGLossless:  settings.OptimizePNGLossless,
			OnProgress: func(doneFiles, totalFiles int, doneBytes, totalBytes int64) {
				pct := 5 + (doneFiles*20)/maxInt(totalFiles, 1)
				elapsed := time.Since(packStart)
				etaStr := "calculando..."
				spdStr := ""
				if elapsed > 200*time.Millisecond && doneBytes > 0 {
					bps := float64(doneBytes) / elapsed.Seconds()
					spdStr = fmt.Sprintf("%.1f MB/s", bps/(1024*1024))
					if totalBytes > doneBytes {
						rem := time.Duration(float64(totalBytes-doneBytes)/bps) * time.Second
						etaStr = fmtETA(rem)
					}
				}
				s.updateJobETA(job, pct, fmt.Sprintf("Empacotando %d/%d arquivo(s)...", doneFiles, totalFiles), etaStr, spdStr)
			},
		})
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		manifest.CreatedAt = time.Now().Format(time.RFC3339)

		manifestJSON, err := packer.MarshalManifest(manifest)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		var (
			wrappedManifest []byte
			encPayload      []byte
			keyScope        string
			usedFolderKey   string
		)
		if len(req.PublicPasswords) > 0 {
			cleanPWs, pwErr := crypto.NormalizePublicPasswords(req.PublicPasswords)
			if pwErr != nil {
				s.finishJob(job, nil, pwErr)
				return
			}
			s.updateJobETA(job, 28, fmt.Sprintf("Criptografando Modo Público (%d camada(s) Pós-Quântica · independente do cofre)...", len(cleanPWs)), "alguns segundos...", "")
			encManifest, err := crypto.EncryptPublicCascade(cleanPWs, contSalt, manifestJSON, []byte("YTCloud-Public-Manifest:"+contUUID))
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}
			encPayload, err = crypto.EncryptPublicCascade(cleanPWs, contSalt, rawPayload, []byte("YTCloud-Public-Payload:"+contUUID))
			rawPayload = nil
			runtime.GC()
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}
			wrappedManifest = crypto.WrapPublicManifest(len(cleanPWs), contSalt, contUUID, encManifest)
		} else {
			s.updateJobETA(job, 28, "Criptografando com Chave da Pasta + Cascata Pós-Quântica (AES-256-GCM + XChaCha20-Poly1305)...", "alguns segundos...", "")
			fkHex, fkErr := s.store.EnsureAndGetFolderKey(folderID)
			if fkErr != nil {
				s.finishJob(job, nil, fkErr)
				return
			}
			contKey, err := vault.DeriveFolderContainerKey(fkHex, contSalt)
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}
			defer crypto.ZeroBytes(contKey)
			keyScope = "folder"
			usedFolderKey = fkHex

			encManifest, err := crypto.EncryptPQ(contKey, manifestJSON, []byte("YTCloud-Manifest:"+contUUID))
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}
			encPayload, err = crypto.EncryptPQ(contKey, rawPayload, []byte("YTCloud-Payload:"+contUUID))
			rawPayload = nil // free plaintext buffer immediately to reduce peak RAM!
			runtime.GC()
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}
			wrappedManifest = make([]byte, 0, 16+len(contUUID)+len(encManifest))
			wrappedManifest = append(wrappedManifest, contSalt...)
			wrappedManifest = append(wrappedManifest, []byte(contUUID)...)
			wrappedManifest = append(wrappedManifest, encManifest...)
		}

		outExt := ".mp4"
		resolvedVCodec := "H.264"
		if pObj, pErr := codec.ProfileByCode(profCode); pErr == nil {
			outExt = codec.OutputExtensionForCodec(videoCodec, pObj, tools.FFmpegPath)
			switch codec.ResolveVideoCodec(videoCodec, pObj, tools.FFmpegPath) {
			case "av1":
				resolvedVCodec = "AV1"
			case "vp9":
				resolvedVCodec = "VP9"
			case "hevc":
				resolvedVCodec = "HEVC"
			}
		}
		outVideo := filepath.Join(s.stagingDir, fileBase+outExt)

		// Optional external host video & audio resolution (local file or URL via yt-dlp)
		var hostVideoPath, customAudioPath string
		if strings.TrimSpace(req.HostVideoSource) != "" || (req.AudioMode == "file" && strings.TrimSpace(req.AudioSource) != "") {
			mediaTmpDir := filepath.Join(s.stagingDir, "media_tmp", contUUID)
			defer os.RemoveAll(mediaTmpDir)
			ytOpts := s.ytDlpOpts()
			if strings.TrimSpace(req.HostVideoSource) != "" {
				s.updateJobETA(job, 31, "Preparando vídeo portador (arquivo ou link)...", "alguns segundos...", "")
				hp, err := resolveMediaInput(tools, ytOpts, req.HostVideoSource, mediaTmpDir, false)
				if err != nil {
					s.finishJob(job, nil, err)
					return
				}
				hostVideoPath = hp
			}
			if req.AudioMode == "file" && strings.TrimSpace(req.AudioSource) != "" {
				s.updateJobETA(job, 33, "Preparando trilha de áudio (arquivo ou link)...", "alguns segundos...", "")
				ap, err := resolveMediaInput(tools, ytOpts, req.AudioSource, mediaTmpDir, true)
				if err != nil {
					s.finishJob(job, nil, err)
					return
				}
				customAudioPath = ap
			}
		}

		// Compute mandatory target frames when DurationSource is "video" or "audio"
		targetFrames := 0
		durSrc := strings.ToLower(strings.TrimSpace(req.DurationSource))
		if durSrc == "video" && hostVideoPath != "" {
			if dur, dErr := codec.ProbeMediaDuration(tools.FFprobePath, tools.FFmpegPath, hostVideoPath); dErr == nil && dur > 0 {
				targetFrames = int(math.Round(dur * float64(fps)))
			}
		} else if durSrc == "audio" {
			probeTarget := customAudioPath
			if probeTarget == "" && req.AudioMode == "host" {
				probeTarget = hostVideoPath
			}
			if probeTarget != "" {
				if dur, dErr := codec.ProbeMediaDuration(tools.FFprobePath, tools.FFmpegPath, probeTarget); dErr == nil && dur > 0 {
					targetFrames = int(math.Round(dur * float64(fps)))
				}
			}
		}

		encStart := time.Now()
		s.updateJobETA(job, 35, fmt.Sprintf("Codificando frames ópticos %s (%s · %s) em segundo plano...", strings.ToUpper(resCode), profCode, resolvedVCodec), "calculando...", "")

		stats, err := codec.EncodeStreamToVideo(codec.VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outVideo,
			ResolutionCode: resCode,
			ProfileCode:    profCode,
			VideoCodec:     videoCodec,
			FPS:            fps,
			CRF:            0, // auto optimal CRF per profile & codec
			Threads:        cpuThreads,
			LowPriority:    settings.BackgroundMode,
			ContainerID:    contNumericID,
			ArtTheme:       resolvedTheme,
			MaskIntensity:  codec.ParseMaskIntensity(req.MaskIntensity),
			HideHeaderBar:  req.HideHeaderBar,
			HostVideoPath:  hostVideoPath,
			AudioMode:      req.AudioMode,
			AudioPath:      customAudioPath,
			MusicOpts: codec.MusicOptions{
				Style:       req.MusicStyle,
				Mood:        req.MusicMood,
				Instruments: req.MusicInstruments,
				Prompt:      req.MusicPrompt,
				Seed:        req.MusicSeed,
			},
			TargetFrames: targetFrames,
			PaddingMode:  req.PaddingMode,
			CrossFECMode: req.CrossFECMode,
			OnProgress: func(frame, total int) {
				pct := 35 + (frame*50)/maxInt(total, 1)
				elapsed := time.Since(encStart)
				etaStr := "calculando..."
				spdStr := ""
				if elapsed > 200*time.Millisecond && frame > 0 {
					fpsRate := float64(frame) / elapsed.Seconds()
					spdStr = fmt.Sprintf("%.1f fps", fpsRate)
					if total > frame && fpsRate > 0 {
						rem := time.Duration(float64(total-frame)/fpsRate) * time.Second
						etaStr = fmtETA(rem)
					}
				}
				s.updateJobETA(job, pct, fmt.Sprintf("Renderizando frame %d/%d (%s %s · %s)", frame, total, strings.ToUpper(resCode), profCode, resolvedVCodec), etaStr, spdStr)
			},
		}, wrappedManifest, encPayload)
		encPayload = nil
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		resolvedPrivacy := normalizePrivacy(req.Privacy)
		if resolvedPrivacy == "" {
			resolvedPrivacy = normalizePrivacy(settings.DefaultPrivacy)
		}
		if resolvedPrivacy == "" {
			resolvedPrivacy = "unlisted"
		}
		container := &vault.VideoContainer{
			ID:             contUUID,
			ShortCode:      shortCode,
			LocalVideoPath: outVideo,
			Resolution:     resCode,
			ProfileCode:    profCode,
			FPS:            fps,
			TotalFrames:    stats.TotalFrames,
			ManifestFrames: stats.ManifestFrames,
			VideoSizeBytes: stats.VideoSizeBytes,
			DataSizeBytes:  manifest.TotalSize,
			FileCount:      len(manifest.Files),
			Status:         "staged",
			Privacy:        resolvedPrivacy,
			ContainerSalt:  contSalt,
			KeyScope:       keyScope,
			FolderID:       folderID,
			FolderKeyHex:   usedFolderKey,
			CreatedAt:      time.Now().Format(time.RFC3339),
		}

		// Optional automatic upload via YouTube Data API v3
		if autoUploadYT {
			if tok, err := s.ensureYouTubeAccessToken(); err == nil && tok != "" {
				s.updateJob(job, 88, "Enviando vídeo para o YouTube via API v3...")
				vid, upErr := youtube.UploadVideoAPI(tok, outVideo, shortCode, "YTCloud Encrypted Container", resolvedPrivacy)
				if upErr == nil {
					container.YouTubeVideoID = vid
					container.YouTubeURL = "https://www.youtube.com/watch?v=" + vid
					container.Status = "uploaded"
					if settings.CleanupPolicy == "immediate_after_upload" {
						if os.Remove(outVideo) == nil {
							container.LocalDeleted = true
						}
					}
				} else {
					s.updateJob(job, 92, "Upload API falhou ("+upErr.Error()+") — vídeo salvo localmente para envio manual.")
				}
			}
		}

		// Save container & all virtual folders/files into encrypted vault catalog
		now := time.Now().Format(time.RFC3339)
		err = s.store.Update(func(d *vault.VaultData) error {
			d.Containers[container.ID] = container
			for _, pf := range manifest.Files {
				dirPart := filepath.Dir(pf.RelPath)
				targetFolder := folderID
				if dirPart != "." && dirPart != "/" && dirPart != "" {
					targetFolder = s.store.EnsureFolderPathLocked(d, folderID, dirPart)
				}
				fid, _ := crypto.RandomHex(6)
				effDate := pf.ModTime
				prec := "exact"
				if effDate == "" {
					prec = "unknown"
				}
				d.Files[fid] = &vault.FileEntry{
					ID:              fid,
					FolderID:        targetFolder,
					ContainerID:     container.ID,
					RelPathInVideo:  pf.RelPath,
					OriginalName:    pf.Name,
					DisplayName:     pf.Name,
					MimeType:        pf.MimeType,
					SizeBytes:       pf.OriginalSize,
					SHA256:          pf.SHA256,
					OriginalDate:    pf.ModTime,
					EffectiveDate:   effDate,
					DatePrecision:   prec,
					PreviewKind:     pf.PreviewKind,
					PreviewBase64:   pf.PreviewBase64,
					LocalSourcePath: pf.LocalPath,
					AddedAt:         now,
				}
			}
			return nil
		})
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		s.finishJob(job, container, nil)
	}()

	return job
}

func (s *Server) handleContainerAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/containers/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		writeError(w, 400, "ação inválida")
		return
	}
	id, action := parts[0], parts[1]

	switch action {
	case "link":
		var req struct {
			YouTubeURL   string `json:"youtube_url"`
			DeleteLocal  bool   `json:"delete_local"`
			VerifyOnline bool   `json:"verify_online"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "JSON inválido")
			return
		}
		vid := youtube.ExtractVideoID(req.YouTubeURL)
		if vid == "" {
			writeError(w, 400, "URL ou ID do YouTube inválido")
			return
		}
		ytURL := "https://www.youtube.com/watch?v=" + vid
		status := "uploaded"
		verifiedAt := ""
		maxH := 0
		if req.VerifyOnline {
			ytOpts := s.ytDlpOpts()
			if ytOpts.YtDlpPath != "" {
				if h, _, err := youtube.CheckVideoAvailable(ytOpts, ytURL); err == nil && h > 0 {
					maxH = h
					status = "verified"
					verifiedAt = time.Now().Format(time.RFC3339)
				}
			}
		}

		var updated *vault.VideoContainer
		err := s.store.Update(func(d *vault.VaultData) error {
			c, ok := d.Containers[id]
			if !ok {
				return errors.New("contêiner não encontrado")
			}
			c.YouTubeVideoID = vid
			c.YouTubeURL = ytURL
			c.Status = status
			if verifiedAt != "" {
				c.VerifiedAt = verifiedAt
			}
			shouldDel := req.DeleteLocal || d.Settings.CleanupPolicy == "immediate_after_upload" ||
				(d.Settings.CleanupPolicy == "safe_after_verify" && status == "verified")
			if shouldDel && c.LocalVideoPath != "" && !c.LocalDeleted {
				if os.Remove(c.LocalVideoPath) == nil {
					c.LocalDeleted = true
				}
			}
			updated = c
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"container": updated, "detected_height": maxH})

	case "cleanup":
		err := s.store.Update(func(d *vault.VaultData) error {
			c, ok := d.Containers[id]
			if !ok {
				return errors.New("contêiner não encontrado")
			}
			if c.LocalVideoPath != "" {
				_ = os.Remove(c.LocalVideoPath)
				c.LocalDeleted = true
			}
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})

	case "upload-yt":
		var contCopy vault.VideoContainer
		var settings vault.VaultSettings
		err := s.store.Read(func(d *vault.VaultData) error {
			c, ok := d.Containers[id]
			if !ok {
				return errors.New("contêiner não encontrado")
			}
			contCopy = *c
			settings = d.Settings
			return nil
		})
		if err != nil {
			writeError(w, 404, err.Error())
			return
		}
		if contCopy.LocalVideoPath == "" || contCopy.LocalDeleted {
			writeError(w, 400, "o arquivo .mp4 local deste vídeo já foi removido")
			return
		}
		if _, err := os.Stat(contCopy.LocalVideoPath); err != nil {
			writeError(w, 400, "arquivo .mp4 local não encontrado em: "+contCopy.LocalVideoPath)
			return
		}
		tok, err := s.ensureYouTubeAccessToken()
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		privacy := normalizePrivacy(contCopy.Privacy)
		if privacy == "" {
			privacy = normalizePrivacy(settings.DefaultPrivacy)
		}
		if privacy == "" {
			privacy = "unlisted"
		}

		job := s.createJob("upload_yt", fmt.Sprintf("Enviando %s para o YouTube", contCopy.ShortCode))
		go func() {
			t0 := time.Now()
			s.updateJobETA(job, 2, "Iniciando upload resumable na YouTube API v3...", "calculando...", "")
			vid, upErr := youtube.UploadVideoAPI(tok, contCopy.LocalVideoPath, contCopy.ShortCode, "YTCloud Encrypted Container", privacy,
				func(sent, total int64) {
					pct := int((sent * 95) / maxInt64(total, 1))
					elapsed := time.Since(t0)
					etaStr, spdStr := "calculando...", ""
					if elapsed > 300*time.Millisecond && sent > 0 {
						bps := float64(sent) / elapsed.Seconds()
						spdStr = fmt.Sprintf("%.1f MB/s", bps/(1024*1024))
						if total > sent {
							etaStr = fmtETA(time.Duration(float64(total-sent)/bps) * time.Second)
						}
					}
					s.updateJobETA(job, pct, fmt.Sprintf("Enviando %.1f MB / %.1f MB para o YouTube...", float64(sent)/(1024*1024), float64(total)/(1024*1024)), etaStr, spdStr)
				})
			if upErr != nil {
				s.finishJob(job, nil, upErr)
				return
			}
			err := s.store.Update(func(d *vault.VaultData) error {
				c, ok := d.Containers[id]
				if !ok {
					return nil
				}
				c.YouTubeVideoID = vid
				c.YouTubeURL = "https://www.youtube.com/watch?v=" + vid
				c.Status = "uploaded"
				if d.Settings.CleanupPolicy == "immediate_after_upload" && c.LocalVideoPath != "" {
					if os.Remove(c.LocalVideoPath) == nil {
						c.LocalDeleted = true
					}
				}
				return nil
			})
			s.finishJob(job, map[string]any{"youtube_video_id": vid, "youtube_url": "https://www.youtube.com/watch?v=" + vid}, err)
		}()
		writeJSON(w, job)

	case "set-privacy":
		var req struct {
			Privacy string `json:"privacy"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		req.Privacy = normalizePrivacy(req.Privacy)
		if req.Privacy == "" {
			req.Privacy = "unlisted"
		}
		var vid string
		_ = s.store.Read(func(d *vault.VaultData) error {
			if c, ok := d.Containers[id]; ok {
				vid = c.YouTubeVideoID
			}
			return nil
		})
		if vid == "" {
			writeError(w, 400, "este vídeo ainda não foi enviado para o YouTube")
			return
		}
		tok, err := s.ensureYouTubeAccessToken()
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := youtube.SetVideoPrivacyAPI(tok, vid, req.Privacy); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		_ = s.store.Update(func(d *vault.VaultData) error {
			if c, ok := d.Containers[id]; ok {
				c.Privacy = req.Privacy
			}
			return nil
		})
		writeJSON(w, map[string]any{"ok": true, "privacy": req.Privacy})

	case "verify":
		var ytURL string
		_ = s.store.Read(func(d *vault.VaultData) error {
			if c, ok := d.Containers[id]; ok {
				ytURL = c.YouTubeURL
			}
			return nil
		})
		if ytURL == "" {
			writeError(w, 400, "este vídeo ainda não tem URL do YouTube vinculada")
			return
		}
		h, _, err := youtube.CheckVideoAvailable(s.ytDlpOpts(), ytURL)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		_ = s.store.Update(func(d *vault.VaultData) error {
			if c, ok := d.Containers[id]; ok {
				c.Status = "verified"
				c.VerifiedAt = time.Now().Format(time.RFC3339)
				if d.Settings.CleanupPolicy == "safe_after_verify" && c.LocalVideoPath != "" && !c.LocalDeleted {
					if os.Remove(c.LocalVideoPath) == nil {
						c.LocalDeleted = true
					}
				}
			}
			return nil
		})
		writeJSON(w, map[string]any{"ok": true, "detected_height": h})

	case "open-folder":
		var localPath string
		_ = s.store.Read(func(d *vault.VaultData) error {
			if c, ok := d.Containers[id]; ok {
				localPath = c.LocalVideoPath
			}
			return nil
		})
		if localPath == "" {
			localPath = s.stagingDir
		}
		if runtime.GOOS == "windows" {
			if _, err := os.Stat(localPath); err == nil {
				_ = exec.Command("explorer.exe", "/select,"+localPath).Start()
			} else {
				_ = exec.Command("explorer.exe", s.stagingDir).Start()
			}
		}
		writeJSON(w, map[string]any{"path": localPath})

	case "set-title":
		s.handleContainerSetTitle(w, r, id)

	case "disassociate":
		s.handleContainerDisassociate(w, r, id)

	case "delete":
		s.handleContainerDelete(w, r, id)

	default:
		writeError(w, 404, "ação não encontrada")
	}
}

// handleExtract downloads (if needed), decodes, decrypts, and extracts selected files or an entire container!
func (s *Server) handleExtract(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ContainerID        string   `json:"container_id"`
		FileIDs            []string `json:"file_ids"`
		DestDir            string   `json:"dest_dir"`
		ApplyEditedMeta    bool     `json:"apply_edited_meta"`
		SkipUnchangedLocal bool     `json:"skip_unchanged_local"`
		VideoSource        string   `json:"video_source"` // optional manual YouTube URL or local .mp4 override
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	if strings.TrimSpace(req.DestDir) == "" {
		home, _ := os.UserHomeDir()
		req.DestDir = filepath.Join(home, "Downloads", "BrightVault-Restaurados")
	}

	dlID, _ := crypto.RandomHex(6)
	pd := &vault.PendingDownload{
		ID:                 dlID,
		Title:              "Restaurando e descompilando arquivos",
		Kind:               "container",
		SourceURL:          req.VideoSource,
		ContainerID:        req.ContainerID,
		FileIDs:            req.FileIDs,
		DestDir:            req.DestDir,
		ApplyEditedMeta:    req.ApplyEditedMeta,
		SkipUnchangedLocal: req.SkipUnchangedLocal,
		DownloadDir:        filepath.Join(s.stagingDir, "downloads", dlID),
		CreatedAt:          time.Now().Format(time.RFC3339),
	}
	job := s.runExtractPendingDownload(pd)
	writeJSON(w, job)
}

func (s *Server) runExtractPendingDownload(pd *vault.PendingDownload) *Job {
	// Persist PendingDownload in vault so yt-dlp (--continue) resumes seamlessly if app is closed mid-download!
	_ = s.store.UpdateSystem(func(d *vault.VaultData) error {
		if d.PendingDownloads == nil {
			d.PendingDownloads = map[string]*vault.PendingDownload{}
		}
		d.PendingDownloads[pd.ID] = pd
		return nil
	})

	job := s.createJob("decode", pd.Title)
	go func() {
		tools := media.DetectTools()
		if !tools.HasFFmpeg {
			s.finishJob(job, nil, errors.New("FFmpeg não encontrado"))
			return
		}

		// Group requested files by ContainerID
		var containerIDs []string
		filesByCont := map[string][]*vault.FileEntry{}
		contMap := map[string]*vault.VideoContainer{}

		err := s.store.Read(func(d *vault.VaultData) error {
			if len(pd.FileIDs) > 0 {
				for _, fid := range pd.FileIDs {
					if f, ok := d.Files[fid]; ok {
						filesByCont[f.ContainerID] = append(filesByCont[f.ContainerID], f)
						if c, ok2 := d.Containers[f.ContainerID]; ok2 {
							contMap[f.ContainerID] = c
						}
					}
				}
			} else if pd.ContainerID != "" {
				c, ok := d.Containers[pd.ContainerID]
				if !ok {
					return errors.New("contêiner não encontrado")
				}
				contMap[c.ID] = c
				for _, f := range d.Files {
					if f.ContainerID == c.ID {
						filesByCont[c.ID] = append(filesByCont[c.ID], f)
					}
				}
			}
			for cid := range contMap {
				containerIDs = append(containerIDs, cid)
			}
			return nil
		})
		if err != nil || len(containerIDs) == 0 {
			s.clearPendingDownload(pd.ID, pd.DownloadDir)
			s.finishJob(job, nil, errors.New("nenhum arquivo ou contêiner selecionado"))
			return
		}

		var allExtracted []string
		skippedUnchanged := 0
		lastSave := time.Now()
		for idx, cid := range containerIDs {
			c := contMap[cid]

			// Incremental Download check: if every requested file in this container already exists
			// locally in DestDir with matching SHA-256, skip downloading this YouTube video completely!
			if pd.SkipUnchangedLocal && len(filesByCont[cid]) > 0 {
				var needed []*vault.FileEntry
				for _, fe := range filesByCont[cid] {
					if localFileMatchesSHA256(pd.DestDir, fe.RelPathInVideo, fe.SHA256) {
						skippedUnchanged++
					} else {
						needed = append(needed, fe)
					}
				}
				if len(needed) == 0 {
					continue
				}
				filesByCont[cid] = needed
			}

			videoPath := pd.SourceURL
			tempDownloaded := false

			if videoPath == "" {
				if c.LocalVideoPath != "" && !c.LocalDeleted {
					if _, err := os.Stat(c.LocalVideoPath); err == nil {
						videoPath = c.LocalVideoPath
					}
				}
			}
			if videoPath == "" && c.YouTubeURL != "" {
				s.updateJobETA(job, 10, fmt.Sprintf("Baixando vídeo %s do YouTube (com suporte a retomada)...", c.ShortCode), "calculando...", "")
				dlOpts := s.ytDlpOpts()
				dlOpts.OnProgress = func(pct int, detail, speed, eta string) {
					mapped := 10 + (pct*30)/100
					s.updateJobETA(job, mapped, "Download YouTube: "+detail, eta, speed)
					if time.Since(lastSave) > 4*time.Second {
						lastSave = time.Now()
						_ = s.store.UpdateSystem(func(d *vault.VaultData) error {
							if p, ok := d.PendingDownloads[pd.ID]; ok {
								p.LastProgress = pct
								p.LastDetail = detail
							}
							return nil
						})
					}
				}
				dlPath, err := youtube.DownloadVideo(dlOpts, c.YouTubeURL, pd.DownloadDir)
				if err != nil {
					s.finishJob(job, nil, err)
					return
				}
				videoPath = dlPath
				tempDownloaded = true
			}
			if videoPath == "" {
				s.finishJob(job, nil, fmt.Errorf("contêiner %s sem arquivo local e sem URL do YouTube", c.ShortCode))
				return
			}

			decStart := time.Now()
			s.updateJobETA(job, 40, fmt.Sprintf("Decodificando frames ópticos + FEC (%d/%d)...", idx+1, len(containerIDs)), "calculando...", "")
			decRes, err := codec.DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, videoPath, false, func(fr, tot int) {
				pct := 40 + (fr*45)/maxInt(tot, 1)
				elapsed := time.Since(decStart)
				etaStr := "calculando..."
				spdStr := ""
				if elapsed > 200*time.Millisecond && fr > 0 {
					rate := float64(fr) / elapsed.Seconds()
					spdStr = fmt.Sprintf("%.1f fps", rate)
					if tot > fr && rate > 0 {
						etaStr = fmtETA(time.Duration(float64(tot-fr)/rate) * time.Second)
					}
				}
				s.updateJobETA(job, pct, fmt.Sprintf("Decodificando frame %d/%d + Reed-Solomon FEC", fr, tot), etaStr, spdStr)
			})
			if tempDownloaded {
				_ = os.Remove(videoPath) // auto-clean downloaded temp video
			}
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}

			if isPub, layers, _, _, _, _ := crypto.ParsePublicManifest(decRes.EncryptedManifest); isPub {
				s.finishJob(job, nil, fmt.Errorf("este vídeo está em Modo Público (%d senha(s)) — use '🌐 Download Público' para extrair com a(s) senha(s)", layers))
				return
			}
			// Parse salt(16) + contUUID(16) + encManifest
			if len(decRes.EncryptedManifest) < 32 {
				s.finishJob(job, nil, errors.New("manifesto do vídeo corrompido"))
				return
			}
			salt := decRes.EncryptedManifest[:16]
			contUUID := string(decRes.EncryptedManifest[16:32])
			encManifest := decRes.EncryptedManifest[32:]

			contKey, err := s.store.ResolveContainerKey(c, salt)
			var manifestJSON []byte
			if err == nil {
				manifestJSON, err = crypto.DecryptAuto(contKey, encManifest, []byte("YTCloud-Manifest:"+contUUID))
				if err != nil {
					crypto.ZeroBytes(contKey)
					contKey = nil
				}
			}
			if contKey == nil {
				contKey, manifestJSON, err = s.store.TryDecryptVaultManifest(salt, contUUID, encManifest)
			}
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}
			manifest, err := packer.UnmarshalManifest(manifestJSON)
			if err != nil {
				crypto.ZeroBytes(contKey)
				s.finishJob(job, nil, err)
				return
			}
			rawPayload, err := crypto.DecryptAuto(contKey, decRes.EncryptedPayload, []byte("YTCloud-Payload:"+contUUID))
			crypto.ZeroBytes(contKey)
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}

			selectedRel := map[string]bool{}
			dateOverrides := map[string]string{}
			if len(pd.FileIDs) > 0 {
				for _, fe := range filesByCont[cid] {
					selectedRel[fe.RelPathInVideo] = true
					if pd.ApplyEditedMeta && fe.EffectiveDate != "" {
						dateOverrides[fe.RelPathInVideo] = fe.EffectiveDate
					}
				}
			} else if pd.ApplyEditedMeta {
				for _, fe := range filesByCont[cid] {
					if fe.EffectiveDate != "" {
						dateOverrides[fe.RelPathInVideo] = fe.EffectiveDate
					}
				}
			}

			s.updateJob(job, 90, "Extraindo arquivos e aplicando metadados/EXIF...")
			extracted, err := packer.ExtractFiles(manifest, rawPayload, pd.DestDir, selectedRel, dateOverrides, tools.FFmpegPath)
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}
			// Persist LastExtractedPath on each extracted FileEntry so "Abrir / Pasta local" works right away!
			if len(extracted) > 0 && len(filesByCont[cid]) > 0 {
				pathByRel := map[string]string{}
				for _, ep := range extracted {
					if rel, rErr := filepath.Rel(pd.DestDir, ep); rErr == nil {
						pathByRel[filepath.ToSlash(rel)] = ep
					}
					pathByRel[filepath.Base(ep)] = ep
				}
				_ = s.store.UpdateSystem(func(d *vault.VaultData) error {
					for _, fe := range filesByCont[cid] {
						f, ok := d.Files[fe.ID]
						if !ok {
							continue
						}
						if ep, hit := pathByRel[filepath.ToSlash(fe.RelPathInVideo)]; hit {
							f.LastExtractedPath = ep
						} else if ep, hit := pathByRel[filepath.Base(fe.RelPathInVideo)]; hit {
							f.LastExtractedPath = ep
						}
					}
					return nil
				})
			}
			allExtracted = append(allExtracted, extracted...)
		}

		s.clearPendingDownload(pd.ID, pd.DownloadDir)
		s.finishJob(job, map[string]any{
			"dest_dir":          pd.DestDir,
			"extracted":         allExtracted,
			"file_ids":          pd.FileIDs,
			"container_ids":     containerIDs,
			"count":             len(allExtracted),
			"skipped_unchanged": skippedUnchanged,
		}, nil)
	}()

	return job
}

func (s *Server) clearPendingDownload(id, dlDir string) {
	_ = s.store.UpdateSystem(func(d *vault.VaultData) error {
		delete(d.PendingDownloads, id)
		return nil
	})
	if dlDir != "" {
		_ = os.RemoveAll(dlDir)
	}
}

// handlePeek implements async Manifest-First Peek: inspects ANY YouTube URL or local optical video
// as a background Job with live progress bar + ETA (preventing HTTP "Failed to fetch" timeouts!).
func (s *Server) handlePeek(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"` // YouTube URL or local .mp4 path
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Source) == "" {
		writeError(w, 400, "informe uma URL do YouTube ou caminho de vídeo local")
		return
	}
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		writeError(w, 400, "FFmpeg necessário para inspecionar vídeos")
		return
	}
	src := strings.Trim(strings.TrimSpace(req.Source), `"`)
	job := s.createJob("peek", "Inspecionando conteúdo do vídeo (Manifest-First)")

	go func() {
		videoPath := src
		tempPeek := false

		if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || youtube.ExtractVideoID(src) != "" {
			if !tools.HasYtDlp {
				s.finishJob(job, nil, errors.New("yt-dlp necessário para inspecionar links do YouTube"))
				return
			}
			s.updateJobETA(job, 8, "Baixando apenas os primeiros segundos do vídeo no YouTube...", "calculando...", "")
			dlOpts := s.ytDlpOpts()
			dlOpts.OnProgress = func(pct int, detail, speed, eta string) {
				mapped := 8 + (pct*42)/100
				s.updateJobETA(job, mapped, "Baixando trecho inicial: "+detail, eta, speed)
			}
			p, err := youtube.DownloadManifestPeek(dlOpts, src, filepath.Join(s.stagingDir, "peek"), 5)
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}
			videoPath = p
			tempPeek = true
		}
		if tempPeek {
			defer os.Remove(videoPath)
		}

		decStart := time.Now()
		s.updateJobETA(job, 55, "Decodificando frames de manifesto + Reed-Solomon FEC...", "alguns segundos...", "")
		decRes, err := codec.DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, videoPath, true, func(fr, tot int) {
			pct := 55 + (fr*30)/maxInt(tot, 1)
			elapsed := time.Since(decStart)
			spd := ""
			if elapsed > 200*time.Millisecond && fr > 0 {
				spd = fmt.Sprintf("%.1f fps", float64(fr)/elapsed.Seconds())
			}
			s.updateJobETA(job, pct, fmt.Sprintf("Lendo frame de manifesto %d...", fr), "quase lá...", spd)
		})
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		if len(decRes.EncryptedManifest) < 32 {
			s.finishJob(job, nil, errors.New("manifesto inválido ou incompatível"))
			return
		}

		if isPub, layers, _, _, _, _ := crypto.ParsePublicManifest(decRes.EncryptedManifest); isPub {
			s.finishJob(job, nil, fmt.Errorf("este vídeo está em Modo Público (%d senha(s)) — clique em '🌐 Download Público' para inspecionar e baixar com a(s) senha(s)", layers))
			return
		}

		s.updateJobETA(job, 90, "Descriptografando manifesto com as chaves do cofre...", "< 1s", "")
		salt := decRes.EncryptedManifest[:16]
		contUUID := string(decRes.EncryptedManifest[16:32])
		encManifest := decRes.EncryptedManifest[32:]

		contKey, manifestJSON, err := s.store.TryDecryptVaultManifest(salt, contUUID, encManifest)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		defer crypto.ZeroBytes(contKey)
		manifest, err := packer.UnmarshalManifest(manifestJSON)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		s.finishJob(job, map[string]any{
			"manifest":       manifest,
			"total_frames":   decRes.TotalFrames,
			"resolution":     fmt.Sprintf("%dx%d", decRes.Width, decRes.Height),
			"profile_code":   decRes.ProfileCode,
			"partial_peeked": tempPeek,
		}, nil)
	}()

	writeJSON(w, job)
}

// --- Atomic Catalog Backup & Restore on YouTube ("Vídeo-Índice") ---

func (s *Server) handleCatalogBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password      string `json:"password"`
		IncludeThumbs bool   `json:"include_thumbs"`
		UploadYouTube bool   `json:"upload_youtube"`
		DeleteOldYT   bool   `json:"delete_old_yt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		writeError(w, 400, "digite sua senha mestra para criptografar o Vídeo-Índice")
		return
	}

	job := s.createJob("catalog_backup", "Backup do Catálogo (Vídeo-Índice no YouTube)")
	go func() {
		tools := media.DetectTools()
		if !tools.HasFFmpeg {
			s.finishJob(job, nil, errors.New("FFmpeg necessário para gerar Vídeo-Índice"))
			return
		}

		s.updateJob(job, 15, "Gerando snapshot criptografado do catálogo...")
		snapBlob, err := s.store.ExportEncryptedSnapshot(req.Password, req.IncludeThumbs)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		var vaultID, oldVID, prevVID, privacy string
		var seq int
		var keepPrev bool
		_ = s.store.Read(func(d *vault.VaultData) error {
			vaultID = d.VaultID
			oldVID = d.LastCatalogVID
			prevVID = d.PrevCatalogVID
			seq = d.CatalogSeq + 1
			keepPrev = d.Settings.CatalogKeepPrevious
			privacy = d.Settings.DefaultPrivacy
			return nil
		})
		if privacy == "" {
			privacy = "private"
		}

		title := fmt.Sprintf("BV-CATALOG-%s-v%d", vaultID[:8], seq)
		metaJSON, _ := json.Marshal(map[string]any{
			"is_catalog":    true,
			"catalog_title": title,
			"vault_short":   vaultID[:8],
			"seq":           seq,
			"created_at":    time.Now().Format(time.RFC3339),
		})

		// Encode in Ultra-Safe 1080p Y-2 profile (small file, ultra-fast processing, maximum FEC survival!)
		outPath := filepath.Join(s.stagingDir, title+".mp4")
		s.updateJob(job, 35, fmt.Sprintf("Codificando %s em perfil Ultra-Seguro...", title))
		stats, err := codec.EncodeStreamToVideo(codec.VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outPath,
			ResolutionCode: "1080p",
			ProfileCode:    "y2-safe",
			VideoCodec:     "h264",
			FPS:            10,
			CRF:            12,
			ContainerID:    0xC47A1060 + uint64(seq),
		}, metaJSON, snapBlob)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		res := map[string]any{
			"title":           title,
			"local_path":      outPath,
			"video_size":      stats.VideoSizeBytes,
			"snapshot_bytes":  len(snapBlob),
			"uploaded":        false,
			"old_deleted_vid": "",
		}

		if req.UploadYouTube {
			tok, err := s.ensureYouTubeAccessToken()
			if err != nil {
				s.finishJob(job, res, fmt.Errorf("vídeo gerado em %s, mas YouTube API não conectada (%v)", outPath, err))
				return
			}
			// ATOMIC ROTATION: 1) Upload NEW first!
			s.updateJob(job, 70, "Enviando NOVO Vídeo-Índice para o YouTube...")
			newVID, err := youtube.UploadVideoAPI(tok, outPath, title, "YTCloud Encrypted Catalog Index", privacy)
			if err != nil {
				s.finishJob(job, res, err)
				return
			}
			res["uploaded"] = true
			res["new_video_id"] = newVID
			res["new_video_url"] = "https://www.youtube.com/watch?v=" + newVID

			// 2) Update vault sequence, pointers & SyncedRevision BEFORE deleting old (without marking Profile dirty!)
			_ = s.store.UpdateSystem(func(d *vault.VaultData) error {
				d.CatalogSeq = seq
				d.PrevCatalogVID = oldVID
				d.LastCatalogVID = newVID
				d.SyncedRevision = d.DataRevision
				return nil
			})

			// 3) ONLY AFTER new upload succeeded: delete old Video-Index!
			if req.DeleteOldYT {
				toDelete := oldVID
				if keepPrev {
					toDelete = prevVID // keep N-1 rollback copy, delete N-2
				}
				if toDelete != "" && toDelete != newVID {
					s.updateJob(job, 90, fmt.Sprintf("Removendo Vídeo-Índice antigo (%s)...", toDelete))
					if delErr := youtube.DeleteVideoAPI(tok, toDelete); delErr == nil {
						res["old_deleted_vid"] = toDelete
					}
				}
			}
			// Clean up local temp Video-Index
			_ = os.Remove(outPath)
			res["local_deleted"] = true
		}

		s.finishJob(job, res, nil)
	}()

	writeJSON(w, job)
}

func (s *Server) handleCatalogExportFile(w http.ResponseWriter, r *http.Request) {
	pass := r.Header.Get("X-Vault-Password")
	if pass == "" {
		writeError(w, 400, "senha obrigatória")
		return
	}
	inc := r.URL.Query().Get("thumbs") != "false"
	blob, err := s.store.ExportEncryptedSnapshot(pass, inc)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="ytcloud-catalogo.ytcat"`)
	_, _ = w.Write(blob)
}

func (s *Server) handleCatalogDiscoverYT(w http.ResponseWriter, r *http.Request) {
	tok, err := s.ensureYouTubeAccessToken()
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	items, err := youtube.FindCatalogVideosAPI(tok)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"catalog_videos": items})
}

func (s *Server) handleCatalogRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source         string `json:"source"`          // YouTube URL, local .mp4 Video-Index, or local .ytcat path
		Base64Snapshot string `json:"base64_snapshot"` // optional direct .ytcat upload from browser
		Password       string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		writeError(w, 400, "informe a origem do catálogo e sua senha mestra")
		return
	}

	var snapBlob []byte
	var err error

	if req.Base64Snapshot != "" {
		snapBlob, err = base64.StdEncoding.DecodeString(req.Base64Snapshot)
		if err != nil {
			writeError(w, 400, "arquivo .ytcat inválido")
			return
		}
	} else {
		src := strings.Trim(strings.TrimSpace(req.Source), `"`)
		if strings.HasSuffix(strings.ToLower(src), ".ytcat") {
			snapBlob, err = os.ReadFile(src)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
		} else {
			tools := media.DetectTools()
			if !tools.HasFFmpeg {
				writeError(w, 400, "FFmpeg necessário para decodificar Vídeo-Índice")
				return
			}
			videoPath := src
			tempDL := false
			if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || youtube.ExtractVideoID(src) != "" {
				p, dlErr := youtube.DownloadVideo(s.ytDlpOpts(), src, filepath.Join(s.stagingDir, "catalog_restore"))
				if dlErr != nil {
					writeError(w, 400, dlErr.Error())
					return
				}
				videoPath = p
				tempDL = true
			}
			if tempDL {
				defer os.Remove(videoPath)
			}
			decRes, decErr := codec.DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, videoPath, false, nil)
			if decErr != nil {
				writeError(w, 400, decErr.Error())
				return
			}
			snapBlob = decRes.EncryptedPayload
		}
	}

	if err := s.store.RestoreFromEncryptedSnapshot(snapBlob, req.Password); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// --- YouTube OAuth, Tools Installer & Calibration ---

func (s *Server) ensureYouTubeAccessToken() (string, error) {
	var clientID, clientSecret, access, refresh string
	var expiry int64
	err := s.store.Read(func(d *vault.VaultData) error {
		clientID = d.Settings.YouTubeClientID
		clientSecret = d.Settings.YouTubeClientSecret
		if d.YouTubeAuth != nil {
			access = d.YouTubeAuth.AccessToken
			refresh = d.YouTubeAuth.RefreshToken
			expiry = d.YouTubeAuth.ExpiryUnix
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if refresh == "" {
		return "", errors.New("conta YouTube não conectada via OAuth (use o Modo Manual Assistido ou conecte nas Configurações)")
	}
	if access != "" && time.Now().Unix() < expiry {
		return access, nil
	}
	tok, err := youtube.RefreshAccessToken(clientID, clientSecret, refresh)
	if err != nil {
		return "", fmt.Errorf("conexão com a API do YouTube expirou (%v) — clique em '🔄 Reconectar YouTube' para renovar", err)
	}
	_ = s.store.Update(func(d *vault.VaultData) error {
		if d.YouTubeAuth == nil {
			d.YouTubeAuth = &vault.YouTubeOAuthState{}
		}
		d.YouTubeAuth.AccessToken = tok.AccessToken
		d.YouTubeAuth.ExpiryUnix = tok.ExpiryUnix
		return nil
	})
	return tok.AccessToken, nil
}

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var clientID, clientSecret string
	_ = s.store.Read(func(d *vault.VaultData) error {
		clientID = d.Settings.YouTubeClientID
		clientSecret = d.Settings.YouTubeClientSecret
		return nil
	})
	if clientID == "" || clientSecret == "" {
		writeError(w, 400, "salve seu YouTube Client ID e Client Secret nas Configurações primeiro")
		return
	}

	job := s.createJob("oauth", "Conectando conta do YouTube (OAuth2)...")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		tok, err := youtube.StartOAuthFlow(ctx, clientID, clientSecret, func(authURL string) {
			s.updateJob(job, 30, "Aguardando autorização no navegador...")
			LaunchDesktopWindow(authURL)
		})
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		err = s.store.Update(func(d *vault.VaultData) error {
			d.YouTubeAuth = &vault.YouTubeOAuthState{
				AccessToken:  tok.AccessToken,
				RefreshToken: tok.RefreshToken,
				ExpiryUnix:   tok.ExpiryUnix,
				ChannelTitle: tok.ChannelTitle,
			}
			return nil
		})
		s.finishJob(job, map[string]any{"channel": tok.ChannelTitle}, err)
	}()
	writeJSON(w, job)
}

func (s *Server) handleInstallTools(w http.ResponseWriter, r *http.Request) {
	job := s.createJob("tools_install", "Instalando FFmpeg e yt-dlp portáteis")
	go func() {
		tools := media.DetectTools()
		if !tools.HasYtDlp {
			s.updateJob(job, 15, "Baixando yt-dlp.exe oficial...")
			if _, err := media.InstallYtDlp(func(m string) { s.updateJob(job, 25, m) }); err != nil {
				s.finishJob(job, nil, err)
				return
			}
		}
		if !tools.HasFFmpeg {
			s.updateJob(job, 45, "Baixando FFmpeg portátil oficial (~85 MB)...")
			if _, _, err := media.InstallFFmpeg(func(m string) { s.updateJob(job, 70, m) }); err != nil {
				s.finishJob(job, nil, err)
				return
			}
		}
		s.finishJob(job, media.DetectTools(), nil)
	}()
	writeJSON(w, job)
}

func (s *Server) handleCalibrate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Resolution string   `json:"resolution"`
		Profiles   []string `json:"profiles"`
		CRF        int      `json:"crf"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Resolution == "" {
		req.Resolution = "1080p"
	}
	if len(req.Profiles) == 0 {
		req.Profiles = []string{"y8-gray", "y6-gray", "y4-gray", "y2-safe", "ycbcr-hex"}
	}
	if req.CRF <= 0 {
		req.CRF = 26
	}

	job := s.createJob("calibrate", fmt.Sprintf("Calibração óptica (%s, CRF %d)", strings.ToUpper(req.Resolution), req.CRF))
	go func() {
		tools := media.DetectTools()
		if !tools.HasFFmpeg {
			s.finishJob(job, nil, errors.New("FFmpeg não encontrado"))
			return
		}
		results := make([]codec.CalibrationResult, 0, len(req.Profiles))
		for i, pc := range req.Profiles {
			s.updateJob(job, (i*100)/len(req.Profiles), fmt.Sprintf("Testando perfil %s sob compressão CRF %d...", pc, req.CRF))
			res := codec.RunLocalCalibration(tools.FFmpegPath, tools.FFprobePath, s.stagingDir, req.Resolution, pc, req.CRF)
			results = append(results, res)
		}
		s.finishJob(job, results, nil)
	}()
	writeJSON(w, job)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	s.jobsMu.RLock()
	defer s.jobsMu.RUnlock()
	writeJSON(w, map[string]any{"jobs": s.jobs})
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

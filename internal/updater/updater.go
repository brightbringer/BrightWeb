package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"ytcloud/internal/sysutil"
)

const AppVersion = "0.7"

type Status struct {
	AppName         string `json:"app_name"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url"`
	DownloadURL     string `json:"download_url,omitempty"`
	AssetName       string `json:"asset_name,omitempty"`
	ReleaseNotes    string `json:"release_notes,omitempty"`
	CheckedAt       string `json:"checked_at,omitempty"`
	RepoURL         string `json:"repo_url"`
	Error           string `json:"error,omitempty"`
}

type Manager struct {
	appName     string
	repoOwner   string
	repoName    string
	exeBaseName string
	cachePath   string
	httpClient  *http.Client

	mu     sync.RWMutex
	status Status
	stopCh chan struct{}
}

func NewManager(appName, repoOwner, repoName, exeBaseName, cacheDir string) *Manager {
	repoURL := fmt.Sprintf("https://github.com/%s/%s", repoOwner, repoName)
	m := &Manager{
		appName:     appName,
		repoOwner:   repoOwner,
		repoName:    repoName,
		exeBaseName: exeBaseName,
		cachePath:   filepath.Join(cacheDir, "update_cache_"+strings.ToLower(appName)+".json"),
		httpClient:  &http.Client{Timeout: 20 * time.Second},
		stopCh:      make(chan struct{}),
		status: Status{
			AppName:        appName,
			CurrentVersion: AppVersion,
			LatestVersion:  AppVersion,
			RepoURL:        repoURL,
			ReleaseURL:     repoURL + "/releases",
		},
	}
	m.loadCache()
	return m
}

// StartScheduler checks on startup (after a brief 2s delay) and once every 24 hours.
func (m *Manager) StartScheduler() {
	go func() {
		select {
		case <-time.After(2 * time.Second):
		case <-m.stopCh:
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = m.CheckNow(ctx)
		cancel()

		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
				_ = m.CheckNow(ctx2)
				cancel2()
			case <-m.stopCh:
				return
			}
		}
	}()
}

func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

func (m *Manager) loadCache() {
	raw, err := os.ReadFile(m.cachePath)
	if err != nil {
		return
	}
	var cached Status
	if json.Unmarshal(raw, &cached) == nil && cached.LatestVersion != "" {
		cached.AppName = m.appName
		cached.CurrentVersion = AppVersion
		cached.RepoURL = fmt.Sprintf("https://github.com/%s/%s", m.repoOwner, m.repoName)
		cached.UpdateAvailable = CompareVersions(cached.LatestVersion, AppVersion) > 0
		m.mu.Lock()
		m.status = cached
		m.mu.Unlock()
	}
}

func (m *Manager) saveCache(st Status) {
	if m.cachePath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.cachePath), 0o755)
	if b, err := json.MarshalIndent(st, "", "  "); err == nil {
		_ = os.WriteFile(m.cachePath, b, 0o600)
	}
}

// CheckNow queries GitHub Releases (falling back to Tags) and updates cached status.
func (m *Manager) CheckNow(ctx context.Context) Status {
	repoURL := fmt.Sprintf("https://github.com/%s/%s", m.repoOwner, m.repoName)
	st := Status{
		AppName:        m.appName,
		CurrentVersion: AppVersion,
		LatestVersion:  AppVersion,
		RepoURL:        repoURL,
		ReleaseURL:     repoURL + "/releases",
		CheckedAt:      time.Now().Format(time.RFC3339),
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", m.repoOwner, m.repoName)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", m.appName+"/"+AppVersion)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		st.Error = err.Error()
		m.mu.Lock()
		m.status.Error = st.Error
		m.status.CheckedAt = st.CheckedAt
		out := m.status
		m.mu.Unlock()
		return out
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var rel struct {
			TagName string `json:"tag_name"`
			Name    string `json:"name"`
			HTMLURL string `json:"html_url"`
			Body    string `json:"body"`
			Assets  []struct {
				Name               string `json:"name"`
				BrowserDownloadURL string `json:"browser_download_url"`
			} `json:"assets"`
		}
		if decErr := json.NewDecoder(resp.Body).Decode(&rel); decErr == nil {
			ver := NormalizeVersion(rel.TagName)
			if ver == "" {
				ver = NormalizeVersion(rel.Name)
			}
			if ver != "" {
				st.LatestVersion = ver
				st.UpdateAvailable = CompareVersions(ver, AppVersion) > 0
			}
			if rel.HTMLURL != "" {
				st.ReleaseURL = rel.HTMLURL
			}
			if len(rel.Body) > 600 {
				st.ReleaseNotes = rel.Body[:600] + "..."
			} else {
				st.ReleaseNotes = rel.Body
			}
			// Pick best Windows executable or zip asset
			wantLow := strings.ToLower(m.exeBaseName)
			var fallbackExeURL, fallbackExeName, zipURL, zipName string
			for _, a := range rel.Assets {
				al := strings.ToLower(a.Name)
				if strings.HasSuffix(al, ".exe") {
					if strings.Contains(al, wantLow) {
						st.DownloadURL = a.BrowserDownloadURL
						st.AssetName = a.Name
						break
					}
					if fallbackExeURL == "" {
						fallbackExeURL = a.BrowserDownloadURL
						fallbackExeName = a.Name
					}
				} else if strings.HasSuffix(al, ".zip") && zipURL == "" {
					if strings.Contains(al, wantLow) || strings.Contains(al, "win") {
						zipURL = a.BrowserDownloadURL
						zipName = a.Name
					}
				}
			}
			if st.DownloadURL == "" && fallbackExeURL != "" {
				st.DownloadURL = fallbackExeURL
				st.AssetName = fallbackExeName
			} else if st.DownloadURL == "" && zipURL != "" {
				st.DownloadURL = zipURL
				st.AssetName = zipName
			}
		}
	} else if resp.StatusCode == http.StatusNotFound {
		// Fallback: check tags if no formal GitHub Release has been published yet
		tagURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/tags?per_page=10", m.repoOwner, m.repoName)
		tReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, tagURL, nil)
		tReq.Header.Set("Accept", "application/vnd.github+json")
		tReq.Header.Set("User-Agent", m.appName+"/"+AppVersion)
		if tResp, tErr := m.httpClient.Do(tReq); tErr == nil {
			defer tResp.Body.Close()
			if tResp.StatusCode == http.StatusOK {
				var tags []struct {
					Name string `json:"name"`
				}
				if json.NewDecoder(tResp.Body).Decode(&tags) == nil {
					best := AppVersion
					for _, tg := range tags {
						v := NormalizeVersion(tg.Name)
						if v != "" && CompareVersions(v, best) > 0 {
							best = v
							st.ReleaseURL = repoURL + "/releases/tag/" + tg.Name
						}
					}
					st.LatestVersion = best
					st.UpdateAvailable = CompareVersions(best, AppVersion) > 0
				}
			}
		}
	}

	m.mu.Lock()
	m.status = st
	m.mu.Unlock()
	m.saveCache(st)
	return st
}

// ApplyUpdate downloads the latest release executable from GitHub, schedules a safe self-swap & relaunch on Windows,
// and calls onShutdown. If the release has no binary asset, opens the release page in the browser instead.
func (m *Manager) ApplyUpdate(ctx context.Context, onShutdown func()) (map[string]any, error) {
	st := m.CheckNow(ctx)
	if !st.UpdateAvailable {
		return map[string]any{
			"updated":         false,
			"already_latest":  true,
			"current_version": st.CurrentVersion,
			"latest_version":  st.LatestVersion,
		}, nil
	}
	if st.DownloadURL == "" {
		_ = sysutil.OpenFileDefault(st.ReleaseURL)
		return map[string]any{
			"updated":             false,
			"opened_release_page": true,
			"release_url":         st.ReleaseURL,
			"latest_version":      st.LatestVersion,
		}, nil
	}

	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	dlClient := &http.Client{Timeout: 5 * time.Minute}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, st.DownloadURL, nil)
	req.Header.Set("User-Agent", m.appName+"/"+AppVersion)
	resp, err := dlClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("erro baixando atualização: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download da atualização falhou (HTTP %d)", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var exeBytes []byte
	if strings.HasSuffix(strings.ToLower(st.AssetName), ".zip") {
		zr, zErr := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if zErr != nil {
			return nil, fmt.Errorf("arquivo zip da atualização inválido: %w", zErr)
		}
		wantLow := strings.ToLower(m.exeBaseName)
		for _, zf := range zr.File {
			nl := strings.ToLower(filepath.Base(zf.Name))
			if strings.HasSuffix(nl, ".exe") && (strings.Contains(nl, wantLow) || len(exeBytes) == 0) {
				rc, oErr := zf.Open()
				if oErr == nil {
					b, _ := io.ReadAll(rc)
					rc.Close()
					if len(b) > 2 && b[0] == 'M' && b[1] == 'Z' {
						exeBytes = b
						if strings.Contains(nl, wantLow) {
							break
						}
					}
				}
			}
		}
		if len(exeBytes) == 0 {
			return nil, errors.New("nenhum executável .exe encontrado dentro do pacote .zip")
		}
	} else {
		if len(raw) < 1024 || raw[0] != 'M' || raw[1] != 'Z' {
			return nil, errors.New("arquivo baixado não é um executável Windows válido")
		}
		exeBytes = raw
	}

	newPath := exePath + ".new"
	if err := os.WriteFile(newPath, exeBytes, 0o755); err != nil {
		return nil, fmt.Errorf("erro gravando atualização em %s: %w", newPath, err)
	}

	if runtime.GOOS == "windows" {
		pid := os.Getpid()
		scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("bright_update_%d.cmd", pid))
		script := fmt.Sprintf(`@echo off
timeout /t 2 /nobreak >nul
:retry
move /y "%s" "%s" >nul 2>&1
if errorlevel 1 (
  timeout /t 1 /nobreak >nul
  goto retry
)
start "" "%s"
del "%%~f0"
`, newPath, exePath, exePath)
		if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
			return nil, err
		}
		cmd := exec.Command("cmd.exe", "/c", scriptPath)
		sysutil.ApplySubprocessAttrs(cmd, false)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		if onShutdown != nil {
			go func() {
				time.Sleep(350 * time.Millisecond)
				onShutdown()
			}()
		}
	}

	return map[string]any{
		"updated":         true,
		"restarting":      true,
		"latest_version":  st.LatestVersion,
		"current_version": st.CurrentVersion,
	}, nil
}

// NormalizeVersion strips leading 'v'/'V' and whitespace.
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	v = strings.TrimSpace(v)
	// Ensure starts with digit
	if v == "" || v[0] < '0' || v[0] > '9' {
		return ""
	}
	return v
}

// CompareVersions compares dotted numeric versions (e.g., "0.8" > "0.7", "0.7.1" > "0.7", "1.0" > "0.9").
func CompareVersions(a, b string) int {
	pa := parseVerParts(NormalizeVersion(a))
	pb := parseVerParts(NormalizeVersion(b))
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		va, vb := 0, 0
		if i < len(pa) {
			va = pa[i]
		}
		if i < len(pb) {
			vb = pb[i]
		}
		if va > vb {
			return 1
		}
		if va < vb {
			return -1
		}
	}
	return 0
}

func parseVerParts(v string) []int {
	if v == "" {
		return nil
	}
	segs := strings.Split(v, ".")
	out := make([]int, len(segs))
	for i, s := range segs {
		n, _ := strconv.Atoi(strings.TrimSpace(s))
		out[i] = n
	}
	return out
}

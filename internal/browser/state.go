package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ytcloud/internal/media"
	"ytcloud/internal/sysutil"
)

type SavedLink struct {
	Title   string `json:"title"`
	Address string `json:"address"`
	At      string `json:"at,omitempty"`
}

type BrowserPrefs struct {
	Theme     string      `json:"theme"`
	Language  string      `json:"language"`
	Bookmarks []SavedLink `json:"bookmarks"`
	History   []SavedLink `json:"history"`
}

func browserStatePath() string {
	return filepath.Join(media.DefaultAppDir(), "brightweb_state.json")
}

func (s *Server) loadPrefsLocked() BrowserPrefs {
	def := BrowserPrefs{
		Theme:     "bright-night",
		Language:  "pt",
		Bookmarks: []SavedLink{},
		History:   []SavedLink{},
	}
	raw, err := os.ReadFile(browserStatePath())
	if err != nil {
		return def
	}
	var p BrowserPrefs
	if json.Unmarshal(raw, &p) != nil {
		return def
	}
	if p.Theme == "" {
		p.Theme = "bright-night"
	}
	if p.Language == "" {
		p.Language = "pt"
	}
	if p.Bookmarks == nil {
		p.Bookmarks = []SavedLink{}
	}
	if p.History == nil {
		p.History = []SavedLink{}
	}
	return p
}

func (s *Server) savePrefsLocked(p BrowserPrefs) {
	if len(p.History) > 80 {
		p.History = p.History[:80]
	}
	if len(p.Bookmarks) > 80 {
		p.Bookmarks = p.Bookmarks[:80]
	}
	dir := filepath.Dir(browserStatePath())
	_ = os.MkdirAll(dir, 0o755)
	if b, err := json.MarshalIndent(p, "", "  "); err == nil {
		tmp := browserStatePath() + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, browserStatePath())
		}
	}
}

func (s *Server) handlePrefs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.mu.RLock()
		p := s.prefs
		s.mu.RUnlock()
		writeJSON(w, p)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		Theme     *string      `json:"theme"`
		Language  *string      `json:"language"`
		Bookmarks *[]SavedLink `json:"bookmarks"`
		History   *[]SavedLink `json:"history"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	s.mu.Lock()
	if req.Theme != nil && strings.TrimSpace(*req.Theme) != "" {
		s.prefs.Theme = strings.TrimSpace(*req.Theme)
	}
	if req.Language != nil && strings.TrimSpace(*req.Language) != "" {
		s.prefs.Language = strings.TrimSpace(*req.Language)
	}
	if req.Bookmarks != nil {
		s.prefs.Bookmarks = *req.Bookmarks
	}
	if req.History != nil {
		s.prefs.History = *req.History
	}
	out := s.prefs
	s.savePrefsLocked(out)
	s.mu.Unlock()
	writeJSON(w, out)
}

func (s *Server) handlePickAudio(w http.ResponseWriter, r *http.Request) {
	s.activeDialogs.Add(1)
	s.lastHeartbeat.Store(time.Now().Unix())
	defer func() {
		s.lastHeartbeat.Store(time.Now().Unix())
		s.activeDialogs.Add(-1)
	}()
	paths, err := sysutil.PickFilesDialog("Selecione um arquivo de áudio (.mp3, .wav, .m4a, .ogg, .flac)")
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if len(paths) == 0 {
		writeJSON(w, map[string]any{"cancelled": true})
		return
	}
	writeJSON(w, map[string]any{"path": paths[0]})
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.updater.Status())
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, s.updater.CheckNow(ctx))
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	res, err := s.updater.ApplyUpdate(ctx, func() { s.TriggerShutdown() })
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, res)
}

package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ytcloud/internal/codec"
	"ytcloud/internal/crypto"
	"ytcloud/internal/media"
	"ytcloud/internal/packer"
	"ytcloud/internal/vault"
	"ytcloud/internal/youtube"
)

func normalizePrivacy(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "public":
		return "public"
	case "private":
		return "private"
	case "unlisted":
		return "unlisted"
	default:
		return ""
	}
}

// collectFolderSubtree returns a set of folderID (plus all descendant subfolders if recursive is true)
// along with each folder's relative path from the root folderID.
func collectFolderSubtree(d *vault.VaultData, rootID string, recursive bool) (map[string]string, int) {
	relPaths := map[string]string{rootID: ""}
	subCount := 0
	if !recursive {
		return relPaths, 0
	}
	changed := true
	for changed {
		changed = false
		for _, sub := range d.Folders {
			if sub.ID == rootID {
				continue
			}
			if parentRel, ok := relPaths[sub.ParentID]; ok {
				if _, seen := relPaths[sub.ID]; !seen {
					if parentRel == "" {
						relPaths[sub.ID] = sub.Name
					} else {
						relPaths[sub.ID] = parentRel + "/" + sub.Name
					}
					subCount++
					changed = true
				}
			}
		}
	}
	return relPaths, subCount
}

// handleFolderProps returns folder metadata, its automatic 256-bit FolderKeyHex, and storage stats.
func (s *Server) handleFolderProps(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "método não permitido")
		return
	}
	// Ensure folder key exists before reading
	_, _ = s.store.EnsureAndGetFolderKey(id)

	var resp map[string]any
	err := s.store.Read(func(d *vault.VaultData) error {
		f, ok := d.Folders[id]
		if !ok {
			return errors.New("pasta não encontrada")
		}
		relSubtree, subCount := collectFolderSubtree(d, id, true)
		var directFiles, totalFiles int
		var totalBytes int64
		contIDs := map[string]bool{}
		for _, fe := range d.Files {
			if _, inTree := relSubtree[fe.FolderID]; !inTree {
				continue
			}
			totalFiles++
			totalBytes += fe.SizeBytes
			if fe.FolderID == id {
				directFiles++
			}
			if fe.ContainerID != "" {
				contIDs[fe.ContainerID] = true
			}
		}
		ytLinked, pendingCount, privateCount := 0, 0, 0
		for cid := range contIDs {
			c, ok := d.Containers[cid]
			if !ok {
				continue
			}
			if c.YouTubeURL != "" || c.YouTubeVideoID != "" {
				ytLinked++
				if c.Privacy == "private" {
					privateCount++
				}
			} else {
				pendingCount++
			}
		}
		resp = map[string]any{
			"id":               f.ID,
			"parent_id":        f.ParentID,
			"name":             f.Name,
			"created_at":       f.CreatedAt,
			"folder_key_hex":   f.FolderKeyHex,
			"direct_files":     directFiles,
			"total_files":      totalFiles,
			"subfolder_count":  subCount,
			"total_bytes":      totalBytes,
			"container_count":  len(contIDs),
			"yt_linked_count":  ytLinked,
			"pending_count":    pendingCount,
			"private_count":    privateCount,
		}
		return nil
	})
	if err != nil {
		writeError(w, 404, err.Error())
		return
	}
	writeJSON(w, resp)
}

// handleFolderShareCode generates a point-in-time "YTFOLDER1:..." share code containing
// the folder's current files, YouTube video links, and exact per-container decryption keys
// (isolated from the Master Vault key and other folders).
func (s *Server) handleFolderShareCode(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		IncludeSubfolders *bool `json:"include_subfolders"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	includeSub := true
	if req.IncludeSubfolders != nil {
		includeSub = *req.IncludeSubfolders
	}

	var (
		folderName    string
		shareFiles    []vault.FolderShareFile
		contSnapshots []*vault.VideoContainer
		contSeen      = map[string]bool{}
		missingYT     int
		privateYT     int
		unlinkedFiles int
	)

	err := s.store.Read(func(d *vault.VaultData) error {
		f, ok := d.Folders[id]
		if !ok {
			return errors.New("pasta não encontrada")
		}
		folderName = f.Name
		relSubtree, _ := collectFolderSubtree(d, id, includeSub)

		for _, fe := range d.Files {
			folderRel, inTree := relSubtree[fe.FolderID]
			if !inTree {
				continue
			}
			if fe.ContainerID == "" {
				unlinkedFiles++
				continue
			}
			c, ok := d.Containers[fe.ContainerID]
			if !ok {
				unlinkedFiles++
				continue
			}
			if !contSeen[c.ID] {
				contSeen[c.ID] = true
				cp := *c
				contSnapshots = append(contSnapshots, &cp)
			}
			shareFiles = append(shareFiles, vault.FolderShareFile{
				Name:          fe.DisplayName,
				RelPath:       fe.RelPathInVideo,
				FolderRelPath: folderRel,
				SizeBytes:     fe.SizeBytes,
				EffectiveDate: fe.EffectiveDate,
				ContainerID:   c.ID,
			})
		}
		return nil
	})
	if err != nil {
		writeError(w, 404, err.Error())
		return
	}
	if len(shareFiles) == 0 {
		writeError(w, 400, "esta pasta não possui arquivos vinculados a vídeos para compartilhar")
		return
	}

	sort.Slice(shareFiles, func(i, j int) bool {
		if shareFiles[i].FolderRelPath != shareFiles[j].FolderRelPath {
			return shareFiles[i].FolderRelPath < shareFiles[j].FolderRelPath
		}
		return strings.ToLower(shareFiles[i].Name) < strings.ToLower(shareFiles[j].Name)
	})

	shareConts := make([]vault.FolderShareContainer, 0, len(contSnapshots))
	for _, c := range contSnapshots {
		ytURL := strings.TrimSpace(c.YouTubeURL)
		if ytURL == "" && c.YouTubeVideoID != "" {
			ytURL = "https://www.youtube.com/watch?v=" + c.YouTubeVideoID
		}
		if ytURL == "" {
			missingYT++
		}
		if c.Privacy == "private" {
			privateYT++
		}
		contKey, kErr := s.store.ResolveContainerKey(c, c.ContainerSalt)
		if kErr != nil {
			writeError(w, 500, "erro derivando chave do contêiner "+c.ShortCode+": "+kErr.Error())
			return
		}
		keyHex := hex.EncodeToString(contKey)
		crypto.ZeroBytes(contKey)

		shareConts = append(shareConts, vault.FolderShareContainer{
			ID:             c.ID,
			ShortCode:      c.ShortCode,
			YouTubeURL:     ytURL,
			YouTubeVideoID: c.YouTubeVideoID,
			Privacy:        c.Privacy,
			KeyHex:         keyHex,
		})
	}

	payload := &vault.FolderSharePayload{
		Version:           1,
		FolderName:        folderName,
		CreatedAt:         time.Now().Format(time.RFC3339),
		IncludeSubfolders: includeSub,
		Containers:        shareConts,
		Files:             shareFiles,
	}
	code, err := vault.EncodeFolderShareCode(payload)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	var warnings []string
	if missingYT > 0 {
		warnings = append(warnings, fmt.Sprintf("%d vídeo(s) ainda estão apenas locais (sem link do YouTube) — envie-os primeiro para que outras pessoas consigam baixar.", missingYT))
	}
	if privateYT > 0 {
		warnings = append(warnings, fmt.Sprintf("%d vídeo(s) estão marcados como Privado no YouTube — altere para Não Listado ou Público em Meus Vídeos para que o destinatário consiga baixar sem seus cookies.", privateYT))
	}
	if unlinkedFiles > 0 {
		warnings = append(warnings, fmt.Sprintf("%d arquivo(s) desassociado(s) sem vídeo foram ignorados no código.", unlinkedFiles))
	}

	writeJSON(w, map[string]any{
		"code":             code,
		"folder_name":      folderName,
		"created_at":       payload.CreatedAt,
		"file_count":       len(shareFiles),
		"container_count":  len(shareConts),
		"missing_yt_count": missingYT,
		"private_yt_count": privateYT,
		"warnings":         warnings,
	})
}

// handlePublicFolderInspect parses a "YTFOLDER1:..." folder share code (works WITHOUT any unlocked vault!)
// and returns the list of files and linked videos in that point-in-time snapshot.
func (s *Server) handlePublicFolderInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Code) == "" {
		writeError(w, 400, "cole um código de pasta (YTFOLDER1:...)")
		return
	}
	payload, err := vault.DecodeFolderShareCode(req.Code)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}

	var totalBytes int64
	for _, f := range payload.Files {
		totalBytes += f.SizeBytes
	}
	safeConts := make([]map[string]any, 0, len(payload.Containers))
	missingYT := 0
	for _, c := range payload.Containers {
		if strings.TrimSpace(c.YouTubeURL) == "" {
			missingYT++
		}
		safeConts = append(safeConts, map[string]any{
			"id":               c.ID,
			"short_code":       c.ShortCode,
			"youtube_url":      c.YouTubeURL,
			"youtube_video_id": c.YouTubeVideoID,
			"privacy":          c.Privacy,
			"has_key":          len(c.KeyHex) == 64,
		})
	}

	writeJSON(w, map[string]any{
		"folder_name":        payload.FolderName,
		"created_at":         payload.CreatedAt,
		"include_subfolders": payload.IncludeSubfolders,
		"file_count":         len(payload.Files),
		"container_count":    len(payload.Containers),
		"total_bytes":        totalBytes,
		"missing_yt_count":   missingYT,
		"files":              payload.Files,
		"containers":         safeConts,
	})
}

// handlePublicFolderExtract downloads, decodes, decrypts (using the container keys embedded in YTFOLDER1:...),
// and extracts all (or selected) files from a shared folder snapshot — works WITHOUT any unlocked vault!
func (s *Server) handlePublicFolderExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		Code    string `json:"code"`
		DestDir string `json:"dest_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Code) == "" {
		writeError(w, 400, "cole um código de pasta (YTFOLDER1:...)")
		return
	}
	payload, err := vault.DecodeFolderShareCode(req.Code)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		writeError(w, 400, "FFmpeg necessário para decodificar os vídeos")
		return
	}
	if !tools.HasYtDlp {
		writeError(w, 400, "yt-dlp necessário para baixar os vídeos do YouTube")
		return
	}
	destDir := strings.Trim(strings.TrimSpace(req.DestDir), `"`)
	if destDir == "" {
		home, _ := os.UserHomeDir()
		safeFolder := strings.Map(func(r rune) rune {
			if strings.ContainsRune(`<>:"/\|?*`, r) {
				return '_'
			}
			return r
		}, strings.TrimSpace(payload.FolderName))
		if safeFolder == "" {
			safeFolder = "Pasta-Compartilhada"
		}
		destDir = filepath.Join(home, "Downloads", "BrightVault-Compartilhado", safeFolder)
	}

	job := s.createJob("public_folder_extract", fmt.Sprintf("Baixando pasta compartilhada '%s'", payload.FolderName))
	go func() {
		dlID, _ := crypto.RandomHex(5)
		tempDir := filepath.Join(s.stagingDir, "folder_share_dl", dlID)
		defer os.RemoveAll(tempDir)

		filesByCont := map[string][]vault.FolderShareFile{}
		for _, f := range payload.Files {
			filesByCont[f.ContainerID] = append(filesByCont[f.ContainerID], f)
		}

		var activeConts []vault.FolderShareContainer
		for _, c := range payload.Containers {
			if len(filesByCont[c.ID]) > 0 {
				activeConts = append(activeConts, c)
			}
		}
		if len(activeConts) == 0 {
			s.finishJob(job, nil, errors.New("nenhum arquivo disponível para baixar neste código"))
			return
		}

		var allExtracted []string
		totalC := len(activeConts)
		for idx, c := range activeConts {
			basePct := (idx * 95) / totalC
			spanPct := 95 / totalC
			if spanPct < 10 {
				spanPct = 10
			}
			if strings.TrimSpace(c.YouTubeURL) == "" {
				s.finishJob(job, nil, fmt.Errorf("o vídeo %s não possui link do YouTube neste código de compartilhamento", c.ShortCode))
				return
			}
			contKey, kErr := hex.DecodeString(strings.TrimSpace(c.KeyHex))
			if kErr != nil || len(contKey) != 32 {
				s.finishJob(job, nil, fmt.Errorf("chave do vídeo %s inválida no código de compartilhamento", c.ShortCode))
				return
			}

			s.updateJobETA(job, basePct+2, fmt.Sprintf("Baixando vídeo %d/%d (%s) do YouTube...", idx+1, totalC, c.ShortCode), "calculando...", "")
			dlOpts := s.ytDlpOptsPublic()
			dlOpts.OnProgress = func(pct int, detail, speed, eta string) {
				mapped := basePct + (pct*spanPct)/200
				s.updateJobETA(job, mapped, fmt.Sprintf("Vídeo %d/%d: %s", idx+1, totalC, detail), eta, speed)
			}
			videoPath, dlErr := youtube.DownloadVideo(dlOpts, c.YouTubeURL, filepath.Join(tempDir, c.ID))
			if dlErr != nil {
				crypto.ZeroBytes(contKey)
				s.finishJob(job, nil, fmt.Errorf("erro baixando vídeo %s: %w", c.ShortCode, dlErr))
				return
			}

			decStart := time.Now()
			s.updateJobETA(job, basePct+spanPct/2, fmt.Sprintf("Decodificando frames ópticos (%d/%d)...", idx+1, totalC), "calculando...", "")
			decRes, decErr := codec.DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, videoPath, false, func(fr, tot int) {
				mapped := basePct + spanPct/2 + (fr*(spanPct/2-2))/maxInt(tot, 1)
				el := time.Since(decStart)
				spd, eta := "", "calculando..."
				if el > 200*time.Millisecond && fr > 0 {
					rate := float64(fr) / el.Seconds()
					spd = fmt.Sprintf("%.1f fps", rate)
					if tot > fr && rate > 0 {
						eta = fmtETA(time.Duration(float64(tot-fr)/rate) * time.Second)
					}
				}
				s.updateJobETA(job, mapped, fmt.Sprintf("Vídeo %d/%d · Frame %d/%d + FEC", idx+1, totalC, fr, tot), eta, spd)
			})
			_ = os.Remove(videoPath)
			if decErr != nil {
				crypto.ZeroBytes(contKey)
				s.finishJob(job, nil, decErr)
				return
			}
			if len(decRes.EncryptedManifest) < 32 {
				crypto.ZeroBytes(contKey)
				s.finishJob(job, nil, errors.New("manifesto do vídeo corrompido"))
				return
			}
			contUUID := string(decRes.EncryptedManifest[16:32])
			encManifest := decRes.EncryptedManifest[32:]

			manifestJSON, mErr := crypto.DecryptAuto(contKey, encManifest, []byte("YTCloud-Manifest:"+contUUID))
			if mErr != nil {
				crypto.ZeroBytes(contKey)
				s.finishJob(job, nil, fmt.Errorf("falha ao descriptografar manifesto de %s: %w", c.ShortCode, mErr))
				return
			}
			manifest, uErr := packer.UnmarshalManifest(manifestJSON)
			if uErr != nil {
				crypto.ZeroBytes(contKey)
				s.finishJob(job, nil, uErr)
				return
			}
			rawPayload, pErr := crypto.DecryptAuto(contKey, decRes.EncryptedPayload, []byte("YTCloud-Payload:"+contUUID))
			crypto.ZeroBytes(contKey)
			if pErr != nil {
				s.finishJob(job, nil, fmt.Errorf("falha ao descriptografar dados de %s: %w", c.ShortCode, pErr))
				return
			}

			selectedRel := map[string]bool{}
			dateOverrides := map[string]string{}
			for _, sf := range filesByCont[c.ID] {
				selectedRel[sf.RelPath] = true
				if sf.EffectiveDate != "" {
					dateOverrides[sf.RelPath] = sf.EffectiveDate
				}
			}

			extracted, exErr := packer.ExtractFiles(manifest, rawPayload, destDir, selectedRel, dateOverrides, tools.FFmpegPath)
			if exErr != nil {
				s.finishJob(job, nil, exErr)
				return
			}
			allExtracted = append(allExtracted, extracted...)
		}

		s.finishJob(job, map[string]any{
			"dest_dir":    destDir,
			"folder_name": payload.FolderName,
			"extracted":   allExtracted,
			"count":       len(allExtracted),
		}, nil)
	}()

	writeJSON(w, job)
}

// handleContainersUploadAll uploads all staged containers (that still have a local video file) to YouTube sequentially.
func (s *Server) handleContainersUploadAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var pending []vault.VideoContainer
	var settings vault.VaultSettings
	err := s.store.Read(func(d *vault.VaultData) error {
		settings = d.Settings
		for _, c := range d.Containers {
			if c.Status == "staged" && !c.LocalDeleted && c.LocalVideoPath != "" {
				if _, stErr := os.Stat(c.LocalVideoPath); stErr == nil {
					pending = append(pending, *c)
				}
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, 401, err.Error())
		return
	}
	if len(pending) == 0 {
		writeError(w, 400, "nenhum vídeo pendente com arquivo local pronto para enviar")
		return
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].CreatedAt < pending[j].CreatedAt })

	tok, err := s.ensureYouTubeAccessToken()
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}

	job := s.createJob("upload_yt_all", fmt.Sprintf("Enviando %d vídeo(s) pendente(s) para o YouTube", len(pending)))
	go func() {
		uploaded := 0
		var failures []string
		total := len(pending)
		for i, c := range pending {
			privacy := normalizePrivacy(c.Privacy)
			if privacy == "" {
				privacy = normalizePrivacy(settings.DefaultPrivacy)
			}
			if privacy == "" {
				privacy = "unlisted"
			}
			basePct := (i * 100) / total
			spanPct := 100 / total
			t0 := time.Now()
			s.updateJobETA(job, basePct+1, fmt.Sprintf("[%d/%d] Iniciando upload de %s...", i+1, total, c.ShortCode), "calculando...", "")
			vid, upErr := youtube.UploadVideoAPI(tok, c.LocalVideoPath, c.ShortCode, "YTCloud Encrypted Container", privacy,
				func(sent, totBytes int64) {
					pct := basePct + int((sent*int64(maxInt(spanPct-2, 1)))/maxInt64(totBytes, 1))
					el := time.Since(t0)
					spd, eta := "", "calculando..."
					if el > 300*time.Millisecond && sent > 0 {
						bps := float64(sent) / el.Seconds()
						spd = fmt.Sprintf("%.1f MB/s", bps/(1024*1024))
						if totBytes > sent {
							eta = fmtETA(time.Duration(float64(totBytes-sent)/bps) * time.Second)
						}
					}
					s.updateJobETA(job, pct, fmt.Sprintf("[%d/%d] %s: %.1f / %.1f MB", i+1, total, c.ShortCode, float64(sent)/(1024*1024), float64(totBytes)/(1024*1024)), eta, spd)
				})
			if upErr != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", c.ShortCode, upErr))
				continue
			}
			uploaded++
			cid := c.ID
			_ = s.store.Update(func(d *vault.VaultData) error {
				if cont, ok := d.Containers[cid]; ok {
					cont.YouTubeVideoID = vid
					cont.YouTubeURL = "https://www.youtube.com/watch?v=" + vid
					cont.Status = "uploaded"
					cont.Privacy = privacy
					if d.Settings.CleanupPolicy == "immediate_after_upload" && cont.LocalVideoPath != "" {
						if os.Remove(cont.LocalVideoPath) == nil {
							cont.LocalDeleted = true
						}
					}
				}
				return nil
			})
		}
		if len(failures) > 0 && uploaded == 0 {
			s.finishJob(job, nil, errors.New(strings.Join(failures, "; ")))
			return
		}
		var finalErr error
		if len(failures) > 0 {
			finalErr = fmt.Errorf("%d enviado(s), %d falha(s): %s", uploaded, len(failures), strings.Join(failures, "; "))
		}
		s.finishJob(job, map[string]any{"uploaded": uploaded, "total": total, "failures": failures}, finalErr)
	}()

	writeJSON(w, job)
}

// handleContainerDisassociate detaches all vault files linked to container id (keeping their catalog entries).
func (s *Server) handleContainerDisassociate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		RemoveContainer bool `json:"remove_container"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	detached := 0
	err := s.store.Update(func(d *vault.VaultData) error {
		c, ok := d.Containers[id]
		if !ok {
			return errors.New("vídeo não encontrado")
		}
		for _, fe := range d.Files {
			if fe.ContainerID == id {
				fe.ContainerID = ""
				detached++
			}
		}
		c.FileCount = 0
		if req.RemoveContainer {
			delete(d.Containers, id)
			reconcileSyncGroupsAfterContainerRemovalLocked(d, id, c.SyncID)
		}
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "detached_files": detached})
}

// handleContainerDelete removes a video container from the vault, with explicit options for local .mp4,
// YouTube deletion, and whether associated files should be disassociated or deleted.
func (s *Server) handleContainerDelete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		DeleteLocal bool   `json:"delete_local"`
		DeleteYT    bool   `json:"delete_yt"`
		FileMode    string `json:"file_mode"` // "disassociate" (default) or "delete"
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.FileMode != "delete" {
		req.FileMode = "disassociate"
	}

	var (
		ytVideoID     string
		localPath     string
		localDeleted  bool
		detachedFiles int
		deletedFiles  int
	)
	err := s.store.Update(func(d *vault.VaultData) error {
		c, ok := d.Containers[id]
		if !ok {
			return errors.New("vídeo não encontrado")
		}
		ytVideoID = c.YouTubeVideoID
		localPath = c.LocalVideoPath
		localDeleted = c.LocalDeleted
		syncID := c.SyncID

		for fid, fe := range d.Files {
			if fe.ContainerID != id {
				continue
			}
			if req.FileMode == "delete" {
				delete(d.Files, fid)
				deletedFiles++
			} else {
				fe.ContainerID = ""
				detachedFiles++
			}
		}
		delete(d.Containers, id)
		reconcileSyncGroupsAfterContainerRemovalLocked(d, id, syncID)
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}

	removedLocal := false
	if req.DeleteLocal && localPath != "" && !localDeleted {
		if os.Remove(localPath) == nil {
			removedLocal = true
		}
	}
	ytDeleted := false
	ytErrStr := ""
	if req.DeleteYT && ytVideoID != "" {
		if tok, tokErr := s.ensureYouTubeAccessToken(); tokErr == nil && tok != "" {
			if delErr := youtube.DeleteVideoAPI(tok, ytVideoID); delErr == nil {
				ytDeleted = true
			} else {
				ytErrStr = delErr.Error()
			}
		} else if tokErr != nil {
			ytErrStr = tokErr.Error()
		}
	}

	writeJSON(w, map[string]any{
		"ok":             true,
		"detached_files": detachedFiles,
		"deleted_files":  deletedFiles,
		"removed_local":  removedLocal,
		"yt_deleted":     ytDeleted,
		"yt_error":       ytErrStr,
	})
}

func reconcileSyncGroupsAfterContainerRemovalLocked(d *vault.VaultData, removedContID, syncID string) {
	if syncID == "" || d.SyncGroups == nil {
		return
	}
	sg, ok := d.SyncGroups[syncID]
	if !ok {
		return
	}
	var remaining []*vault.VideoContainer
	for _, c := range d.Containers {
		if c.SyncID == syncID {
			remaining = append(remaining, c)
		}
	}
	if len(remaining) == 0 {
		delete(d.SyncGroups, syncID)
		return
	}
	sort.Slice(remaining, func(i, j int) bool {
		if remaining[i].DeltaIndex != remaining[j].DeltaIndex {
			return remaining[i].DeltaIndex < remaining[j].DeltaIndex
		}
		return remaining[i].CreatedAt < remaining[j].CreatedAt
	})
	sg.BaseContainerID = remaining[0].ID
	sg.LastContainerID = remaining[len(remaining)-1].ID
	maxDelta := 0
	for _, c := range remaining {
		if c.DeltaIndex > maxDelta {
			maxDelta = c.DeltaIndex
		}
	}
	sg.DeltaCount = maxDelta
}

// handleFoldersReorder reorders sibling folders STRICTLY within the same ParentID
// (it never allows pulling a folder out of its parent folder).
func (s *Server) handleFoldersReorder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		ParentID   string   `json:"parent_id"`
		OrderedIDs []string `json:"ordered_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.OrderedIDs) == 0 {
		writeError(w, 400, "lista de pastas inválida")
		return
	}
	if req.ParentID == "" {
		req.ParentID = "root"
	}
	err := s.store.Update(func(d *vault.VaultData) error {
		for _, fid := range req.OrderedIDs {
			f, ok := d.Folders[fid]
			if !ok {
				return errors.New("pasta não encontrada: " + fid)
			}
			p := f.ParentID
			if p == "" {
				p = "root"
			}
			if f.ID == "root" || p != req.ParentID {
				return errors.New("só é permitido reordenar pastas dentro da mesma pasta pai")
			}
		}
		for i, fid := range req.OrderedIDs {
			d.Folders[fid].SortOrder = i + 1
		}
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleFolderWrapParent creates a new parent folder ("Pasta Mãe") at the same level as folder id
// and nests folder id inside that newly created parent folder.
func (s *Server) handleFolderWrapParent(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	if id == "" || id == "root" {
		writeError(w, 400, "não é possível criar pasta mãe para a raiz")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeError(w, 400, "informe o nome da nova Pasta Mãe")
		return
	}
	var newParent *vault.Folder
	err := s.store.Update(func(d *vault.VaultData) error {
		target, ok := d.Folders[id]
		if !ok {
			return errors.New("pasta não encontrada")
		}
		oldParent := target.ParentID
		if oldParent == "" {
			oldParent = "root"
		}
		pid, _ := crypto.RandomHex(6)
		fk, _ := crypto.RandomHex(32)
		newParent = &vault.Folder{
			ID:           pid,
			ParentID:     oldParent,
			Name:         strings.TrimSpace(req.Name),
			SortOrder:    target.SortOrder,
			FolderKeyHex: fk,
			CreatedAt:    time.Now().Format(time.RFC3339),
		}
		d.Folders[pid] = newParent
		target.ParentID = pid
		target.SortOrder = 1
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	safeResp := *newParent
	safeResp.FolderKeyHex = ""
	writeJSON(w, map[string]any{"ok": true, "parent_folder": &safeResp})
}

func sanitizeVideoFileBase(title, fallback string) string {
	clean := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, strings.TrimSpace(title))
	clean = strings.Trim(clean, ". ")
	if clean == "" {
		return fallback
	}
	if len(clean) > 64 {
		clean = clean[:64]
	}
	if clean != fallback {
		return clean + "_" + fallback
	}
	return clean
}

// handleContainerSetTitle lets the user customize/rename a container's video title
// (and updates the title on YouTube via API v3 if already uploaded & connected).
func (s *Server) handleContainerSetTitle(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPatch {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Title) == "" {
		writeError(w, 400, "informe o novo nome do vídeo")
		return
	}
	title := strings.TrimSpace(req.Title)
	if len(title) > 95 {
		title = title[:95]
	}
	var ytVID string
	err := s.store.Update(func(d *vault.VaultData) error {
		c, ok := d.Containers[id]
		if !ok {
			return errors.New("vídeo não encontrado")
		}
		c.ShortCode = title
		ytVID = c.YouTubeVideoID
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ytUpdated := false
	ytWarn := ""
	if ytVID != "" {
		if tok, tokErr := s.ensureYouTubeAccessToken(); tokErr == nil && tok != "" {
			if upErr := youtube.SetVideoTitleAPI(tok, ytVID, title); upErr == nil {
				ytUpdated = true
			} else {
				ytWarn = upErr.Error()
			}
		}
	}
	writeJSON(w, map[string]any{"ok": true, "title": title, "yt_updated": ytUpdated, "yt_warning": ytWarn})
}

// handleOAuthReconnect clears any stale/expired OAuth tokens and starts a fresh YouTube OAuth2 consent flow.
func (s *Server) handleOAuthReconnect(w http.ResponseWriter, r *http.Request) {
	_ = s.store.UpdateSystem(func(d *vault.VaultData) error {
		d.YouTubeAuth = nil
		return nil
	})
	s.handleOAuthStart(w, r)
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

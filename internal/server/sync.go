package server

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ytcloud/internal/codec"
	"ytcloud/internal/crypto"
	"ytcloud/internal/media"
	"ytcloud/internal/packer"
	"ytcloud/internal/vault"
	"ytcloud/internal/youtube"
)

func localFileMatchesSHA256(destDir, relPath, expectedSHA string) bool {
	if expectedSHA == "" {
		return false
	}
	cleanRel := filepath.Clean(filepath.FromSlash(relPath))
	if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
		cleanRel = filepath.Base(cleanRel)
	}
	p := filepath.Join(destDir, cleanRel)
	raw, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	return crypto.SHA256Hex(raw) == expectedSHA
}

// handlePeekExtract downloads & decodes ANY YouTube URL or local optical video directly from the
// "Inspecionar Vídeo" modal, extracting all files or only the checked selected_rel_paths!
func (s *Server) handlePeekExtract(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source             string   `json:"source"`
		SelectedRelPaths   []string `json:"selected_rel_paths"`
		DestDir            string   `json:"dest_dir"`
		SkipUnchangedLocal bool     `json:"skip_unchanged_local"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Source) == "" {
		writeError(w, 400, "informe a URL do YouTube ou caminho local do vídeo")
		return
	}
	if strings.TrimSpace(req.DestDir) == "" {
		home, _ := os.UserHomeDir()
		req.DestDir = filepath.Join(home, "Downloads", "YTCloud-Restaurados")
	}
	src := strings.Trim(strings.TrimSpace(req.Source), `"`)
	dlID, _ := crypto.RandomHex(6)
	pd := &vault.PendingDownload{
		ID:                 dlID,
		Title:              "Baixando e descompilando arquivos do vídeo inspecionado",
		Kind:               "peek",
		SourceURL:          src,
		SelectedRelPaths:   req.SelectedRelPaths,
		DestDir:            req.DestDir,
		SkipUnchangedLocal: req.SkipUnchangedLocal,
		DownloadDir:        filepath.Join(s.stagingDir, "downloads", dlID),
		CreatedAt:          time.Now().Format(time.RFC3339),
	}
	job := s.runPeekPendingDownload(pd)
	writeJSON(w, job)
}

func (s *Server) runPeekPendingDownload(pd *vault.PendingDownload) *Job {
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
		videoPath := pd.SourceURL
		lastSave := time.Now()
		if strings.HasPrefix(pd.SourceURL, "http://") || strings.HasPrefix(pd.SourceURL, "https://") || youtube.ExtractVideoID(pd.SourceURL) != "" {
			s.updateJobETA(job, 8, "Baixando vídeo completo do YouTube (com suporte a retomada)...", "calculando...", "")
			dlOpts := s.ytDlpOpts()
			dlOpts.OnProgress = func(pct int, detail, speed, eta string) {
				mapped := 8 + (pct*35)/100
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
			p, err := youtube.DownloadVideo(dlOpts, pd.SourceURL, pd.DownloadDir)
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}
			videoPath = p
		}

		decStart := time.Now()
		s.updateJobETA(job, 45, "Decodificando frames ópticos + Reed-Solomon FEC...", "calculando...", "")
		decRes, err := codec.DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, videoPath, false, func(fr, tot int) {
			pct := 45 + (fr*42)/maxInt(tot, 1)
			elapsed := time.Since(decStart)
			etaStr, spdStr := "calculando...", ""
			if elapsed > 200*time.Millisecond && fr > 0 {
				rate := float64(fr) / elapsed.Seconds()
				spdStr = fmt.Sprintf("%.1f fps", rate)
				if tot > fr && rate > 0 {
					etaStr = fmtETA(time.Duration(float64(tot-fr)/rate) * time.Second)
				}
			}
			s.updateJobETA(job, pct, fmt.Sprintf("Decodificando frame %d/%d + FEC", fr, tot), etaStr, spdStr)
		})
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		if len(decRes.EncryptedManifest) < 32 {
			s.finishJob(job, nil, errors.New("manifesto inválido"))
			return
		}

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
		rawPayload, err := crypto.DecryptAuto(contKey, decRes.EncryptedPayload, []byte("YTCloud-Payload:"+contUUID))
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		selMap := map[string]bool{}
		for _, rp := range pd.SelectedRelPaths {
			selMap[rp] = true
		}
		if pd.SkipUnchangedLocal {
			filtered := map[string]bool{}
			for _, f := range manifest.Files {
				if len(selMap) > 0 && !selMap[f.RelPath] {
					continue
				}
				if !localFileMatchesSHA256(pd.DestDir, f.RelPath, f.SHA256) {
					filtered[f.RelPath] = true
				}
			}
			if len(filtered) == 0 {
				s.clearPendingDownload(pd.ID, pd.DownloadDir)
				s.finishJob(job, map[string]any{"dest_dir": pd.DestDir, "count": 0, "skipped_all": true}, nil)
				return
			}
			selMap = filtered
		}

		s.updateJobETA(job, 92, "Extraindo arquivos verificados (SHA-256)...", "< 1s", "")
		extracted, err := packer.ExtractFiles(manifest, rawPayload, pd.DestDir, selMap, nil, tools.FFmpegPath)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		s.clearPendingDownload(pd.ID, pd.DownloadDir)
		s.finishJob(job, map[string]any{
			"dest_dir":  pd.DestDir,
			"extracted": extracted,
			"count":     len(extracted),
		}, nil)
	}()

	return job
}

// handleDownloadAction handles resuming or canceling persisted interrupted downloads!
func (s *Server) handleDownloadAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/downloads/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeError(w, 400, "ID de download inválido")
		return
	}

	if r.Method == "DELETE" {
		var dlDir string
		_ = s.store.Read(func(d *vault.VaultData) error {
			if p, ok := d.PendingDownloads[id]; ok {
				dlDir = p.DownloadDir
			}
			return nil
		})
		s.clearPendingDownload(id, dlDir)
		writeJSON(w, map[string]any{"ok": true})
		return
	}

	if len(parts) >= 2 && parts[1] == "resume" {
		var pdCopy *vault.PendingDownload
		_ = s.store.Read(func(d *vault.VaultData) error {
			if p, ok := d.PendingDownloads[id]; ok {
				c := *p
				pdCopy = &c
			}
			return nil
		})
		if pdCopy == nil {
			writeError(w, 404, "download pendente não encontrado")
			return
		}
		var job *Job
		if pdCopy.Kind == "peek" {
			job = s.runPeekPendingDownload(pdCopy)
		} else {
			job = s.runExtractPendingDownload(pdCopy)
		}
		writeJSON(w, job)
		return
	}
	writeError(w, 404, "ação não encontrada")
}

// --- Incremental Sync / Delta Update Videos (max 10 updates per chain, file-level self-contained!) ---

type syncDiffFile struct {
	RelPath   string `json:"rel_path"`
	LocalPath string `json:"local_path,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	Size      int64  `json:"size"`
	ModTime   string `json:"mod_time,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

type syncAnalysis struct {
	SyncID             string         `json:"sync_id"`
	FolderID           string         `json:"folder_id"`
	LocalPath          string         `json:"local_path"`
	IsFirstBackup      bool           `json:"is_first_backup"`
	DeltaCount         int            `json:"delta_count"`
	NextDeltaIndex     int            `json:"next_delta_index"`
	MaxDeltas          int            `json:"max_deltas"`
	NeedsConsolidation bool           `json:"needs_consolidation"`
	ParentVideoID      string         `json:"parent_video_id,omitempty"`
	ParentVideoURL     string         `json:"parent_video_url,omitempty"`
	Added              []syncDiffFile `json:"added"`
	Modified           []syncDiffFile `json:"modified"`
	Deleted            []syncDiffFile `json:"deleted"`
	UnchangedCount     int            `json:"unchanged_count"`
	BytesToUpload      int64          `json:"bytes_to_upload"`
	BytesSkipped       int64          `json:"bytes_skipped"`
}

func (s *Server) computeSyncDiff(localPath, folderID, syncID string) (*syncAnalysis, error) {
	localPath = strings.Trim(strings.TrimSpace(localPath), `"`)
	if localPath == "" {
		return nil, errors.New("informe o caminho da pasta local")
	}
	inputs, err := packer.CollectInputsFromPath(localPath)
	if err != nil {
		return nil, fmt.Errorf("erro escaneando pasta local: %w", err)
	}

	var (
		sg             *vault.SyncGroup
		maxDeltas      = 10
		existingByRel  = map[string]*vault.FileEntry{}
		parentVID      string
		parentURL      string
		resolvedFolder = folderID
	)

	err = s.store.Read(func(d *vault.VaultData) error {
		maxDeltas = d.Settings.EffectiveMaxDeltas()
		if syncID != "" {
			sg = d.SyncGroups[syncID]
		}
		if sg == nil {
			for _, cand := range d.SyncGroups {
				if strings.EqualFold(filepath.Clean(cand.LocalPath), filepath.Clean(localPath)) {
					sg = cand
					break
				}
			}
		}
		if sg != nil {
			resolvedFolder = sg.FolderID
			if lc, ok := d.Containers[sg.LastContainerID]; ok {
				parentVID = lc.YouTubeVideoID
				parentURL = lc.YouTubeURL
			}
		}
		if resolvedFolder == "" {
			resolvedFolder = "root"
		}

		// Collect all existing catalog files under resolvedFolder (recursively)
		allowed := map[string]bool{resolvedFolder: true}
		changed := true
		for changed {
			changed = false
			for _, f := range d.Folders {
				if allowed[f.ParentID] && !allowed[f.ID] {
					allowed[f.ID] = true
					changed = true
				}
			}
		}
		for _, fe := range d.Files {
			if allowed[fe.FolderID] {
				existingByRel[fe.RelPathInVideo] = fe
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &syncAnalysis{
		FolderID:       resolvedFolder,
		LocalPath:      localPath,
		MaxDeltas:      maxDeltas,
		ParentVideoID:  parentVID,
		ParentVideoURL: parentURL,
		Added:          []syncDiffFile{},
		Modified:       []syncDiffFile{},
		Deleted:        []syncDiffFile{},
	}
	if sg != nil {
		out.SyncID = sg.ID
		out.DeltaCount = sg.DeltaCount
		if maxDeltas == 0 {
			out.NextDeltaIndex = 0
			out.NeedsConsolidation = false
		} else {
			out.NextDeltaIndex = sg.DeltaCount + 1
			out.NeedsConsolidation = sg.DeltaCount >= maxDeltas
		}
	} else {
		shortHex, _ := crypto.RandomHex(4)
		out.SyncID = "SYNC-" + shortHex
		out.IsFirstBackup = len(existingByRel) == 0
		out.DeltaCount = 0
		out.NextDeltaIndex = 0
		if !out.IsFirstBackup && maxDeltas > 0 {
			out.NextDeltaIndex = 1
		}
	}

	seenRel := map[string]bool{}
	for _, inp := range inputs {
		rel := filepath.ToSlash(inp.RelPath)
		seenRel[rel] = true
		fi, statErr := os.Stat(inp.LocalPath)
		var sz int64
		if statErr == nil {
			sz = fi.Size()
		}

		existing, ok := existingByRel[rel]
		if !ok {
			out.Added = append(out.Added, syncDiffFile{
				RelPath: rel, LocalPath: inp.LocalPath, Size: sz, ModTime: inp.ModTime,
			})
			out.BytesToUpload += sz
			continue
		}

		// Fast path: same size & mod_time -> unchanged without hashing; if size or mod_time differs, confirm via SHA-256!
		if existing.SizeBytes == sz && existing.OriginalDate == inp.ModTime {
			out.UnchangedCount++
			out.BytesSkipped += sz
			continue
		}
		raw, rErr := os.ReadFile(inp.LocalPath)
		if rErr == nil && existing.SHA256 != "" && crypto.SHA256Hex(raw) == existing.SHA256 {
			out.UnchangedCount++
			out.BytesSkipped += sz
			continue
		}
		out.Modified = append(out.Modified, syncDiffFile{
			RelPath: rel, LocalPath: inp.LocalPath, FileID: existing.ID, Size: sz, ModTime: inp.ModTime,
		})
		out.BytesToUpload += sz
	}

	for rel, fe := range existingByRel {
		if !seenRel[rel] {
			out.Deleted = append(out.Deleted, syncDiffFile{
				RelPath: rel, FileID: fe.ID, Size: fe.SizeBytes,
			})
		}
	}

	return out, nil
}

func (s *Server) handleSyncAnalyze(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPath string `json:"local_path"`
		FolderID  string `json:"folder_id"`
		SyncID    string `json:"sync_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	diff, err := s.computeSyncDiff(req.LocalPath, req.FolderID, req.SyncID)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, diff)
}

func (s *Server) handleSyncRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPath    string `json:"local_path"`
		FolderID     string `json:"folder_id"`
		SyncID       string `json:"sync_id"`
		Resolution   string `json:"resolution"`
		Profile      string `json:"profile"`
		FPS          int    `json:"fps"`
		AutoUploadYT bool   `json:"auto_upload_yt"`
		Consolidate  bool   `json:"consolidate"` // true => full fresh Base video resetting delta counter to 0/10
		ApplyDeletes bool   `json:"apply_deletes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	diff, err := s.computeSyncDiff(req.LocalPath, req.FolderID, req.SyncID)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}

	// When MaxDeltas == 0, never create delta update videos — always create full complete videos!
	fullOnly := diff.MaxDeltas == 0

	// Enforce configurable delta limit (1..99) unless consolidating!
	if !fullOnly && !req.Consolidate && !diff.IsFirstBackup && diff.NeedsConsolidation {
		writeError(w, 400, fmt.Sprintf("limite de %d vídeos de atualização atingido para esta pasta — marque 'Consolidar Novo Backup Base' para zerar a cadeia com segurança!", diff.MaxDeltas))
		return
	}

	var toPack []packer.InputFile
	isBase := fullOnly || diff.IsFirstBackup || req.Consolidate
	if isBase {
		allInputs, err := packer.CollectInputsFromPath(diff.LocalPath)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		toPack = allInputs
	} else {
		for _, a := range diff.Added {
			toPack = append(toPack, packer.InputFile{RelPath: a.RelPath, LocalPath: a.LocalPath, ModTime: a.ModTime})
		}
		for _, m := range diff.Modified {
			toPack = append(toPack, packer.InputFile{RelPath: m.RelPath, LocalPath: m.LocalPath, ModTime: m.ModTime})
		}
	}

	deletedRels := make([]string, 0, len(diff.Deleted))
	for _, d := range diff.Deleted {
		deletedRels = append(deletedRels, d.RelPath)
	}

	// If nothing was added or modified (only deletes or already 100% synced)
	if len(toPack) == 0 {
		if req.ApplyDeletes && len(diff.Deleted) > 0 {
			_ = s.store.Update(func(d *vault.VaultData) error {
				affected := map[string]bool{}
				for _, del := range diff.Deleted {
					if fe, ok := d.Files[del.FileID]; ok {
						affected[fe.ContainerID] = true
						delete(d.Files, del.FileID)
					}
				}
				cleanupEmptyContainersLocked(d, affected)
				return nil
			})
			writeJSON(w, map[string]any{"ok": true, "no_video_needed": true, "removed_files": len(diff.Deleted)})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "no_video_needed": true, "message": "Pasta já está 100% sincronizada!"})
		return
	}

	deltaIdx := 0
	titleKind := "Backup Base"
	if !isBase {
		deltaIdx = diff.NextDeltaIndex
		titleKind = fmt.Sprintf("Atualização Delta %d/%d", deltaIdx, diff.MaxDeltas)
	}
	job := s.createJob("encode", fmt.Sprintf("%s (%s · %d arquivo(s))", titleKind, filepath.Base(diff.LocalPath), len(toPack)))

	go func() {
		tools := media.DetectTools()
		if !tools.HasFFmpeg {
			s.finishJob(job, nil, errors.New("FFmpeg não encontrado"))
			return
		}
		var settings vault.VaultSettings
		_ = s.store.Read(func(d *vault.VaultData) error {
			settings = d.Settings
			return nil
		})
		resCode := req.Resolution
		if resCode == "" {
			resCode = settings.DefaultResolution
		}
		profCode := req.Profile
		if profCode == "" {
			profCode = settings.DefaultProfile
		}
		fps := req.FPS
		if fps <= 0 {
			fps = settings.DefaultFPS
		}
		cpuThreads := settings.CPUThreads
		if cpuThreads <= 0 {
			cpuThreads = 2
		}

		packStart := time.Now()
		contUUID, _ := crypto.RandomHex(8)
		shortCode := fmt.Sprintf("YTC-%s-B0", diff.SyncID[5:])
		if !isBase {
			shortCode = fmt.Sprintf("YTC-%s-U%d", diff.SyncID[5:], deltaIdx)
		}
		contSalt, _ := crypto.RandomBytes(16)
		contNumericID := binary.LittleEndian.Uint64(contSalt[:8])

		manifest, rawPayload, err := packer.PackFilesWithOptions(toPack, packer.PackOptions{
			ContainerID:          contUUID,
			ProfileCode:          profCode,
			ResolutionCode:       resCode,
			FFmpegPath:           tools.FFmpegPath,
			IncludeMicroPreviews: true,
			OptimizePNGLossless:  settings.OptimizePNGLossless,
			OnProgress: func(doneFiles, totalFiles int, doneBytes, totalBytes int64) {
				pct := 5 + (doneFiles*20)/maxInt(totalFiles, 1)
				elapsed := time.Since(packStart)
				etaStr, spdStr := "calculando...", ""
				if elapsed > 200*time.Millisecond && doneBytes > 0 {
					bps := float64(doneBytes) / elapsed.Seconds()
					spdStr = fmt.Sprintf("%.1f MB/s", bps/(1024*1024))
					if totalBytes > doneBytes {
						etaStr = fmtETA(time.Duration(float64(totalBytes-doneBytes)/bps) * time.Second)
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
		manifest.SyncID = diff.SyncID
		manifest.SyncFolderName = filepath.Base(diff.LocalPath)
		manifest.DeltaIndex = deltaIdx
		manifest.MaxDeltas = diff.MaxDeltas
		if !isBase {
			manifest.ParentVideoID = diff.ParentVideoID
			manifest.ParentVideoURL = diff.ParentVideoURL
			if req.ApplyDeletes {
				manifest.DeletedRelPaths = deletedRels
			}
		}

		s.updateJobETA(job, 28, "Criptografando pacote incremental com Chave da Pasta + Cascata Pós-Quântica (AES-256 + XChaCha20)...", "alguns segundos...", "")
		fkHex, fkErr := s.store.EnsureAndGetFolderKey(diff.FolderID)
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

		manifestJSON, _ := packer.MarshalManifest(manifest)
		encManifest, err := crypto.EncryptPQ(contKey, manifestJSON, []byte("YTCloud-Manifest:"+contUUID))
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		encPayload, err := crypto.EncryptPQ(contKey, rawPayload, []byte("YTCloud-Payload:"+contUUID))
		rawPayload = nil
		runtime.GC()
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		wrappedManifest := make([]byte, 0, 16+len(contUUID)+len(encManifest))
		wrappedManifest = append(wrappedManifest, contSalt...)
		wrappedManifest = append(wrappedManifest, []byte(contUUID)...)
		wrappedManifest = append(wrappedManifest, encManifest...)

		outVideo := filepath.Join(s.stagingDir, shortCode+".mp4")
		encStart := time.Now()
		stats, err := codec.EncodeStreamToVideo(codec.VideoEncodeOptions{
			FFmpegPath:     tools.FFmpegPath,
			OutputPath:     outVideo,
			ResolutionCode: resCode,
			ProfileCode:    profCode,
			VideoCodec:     settings.DefaultVideoCodec,
			FPS:            fps,
			CRF:            0, // auto optimal CRF per profile & codec
			Threads:        cpuThreads,
			LowPriority:    settings.BackgroundMode,
			ContainerID:    contNumericID,
			OnProgress: func(frame, total int) {
				pct := 35 + (frame*50)/maxInt(total, 1)
				elapsed := time.Since(encStart)
				etaStr, spdStr := "calculando...", ""
				if elapsed > 200*time.Millisecond && frame > 0 {
					rate := float64(frame) / elapsed.Seconds()
					spdStr = fmt.Sprintf("%.1f fps", rate)
					if total > frame && rate > 0 {
						etaStr = fmtETA(time.Duration(float64(total-frame)/rate) * time.Second)
					}
				}
				s.updateJobETA(job, pct, fmt.Sprintf("Renderizando frame %d/%d (%s %s)", frame, total, strings.ToUpper(resCode), profCode), etaStr, spdStr)
			},
		}, wrappedManifest, encPayload)
		encPayload = nil
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		privacy := normalizePrivacy(settings.DefaultPrivacy)
		if privacy == "" {
			privacy = "unlisted"
		}
		container := &vault.VideoContainer{
			ID:              contUUID,
			ShortCode:       shortCode,
			LocalVideoPath:  outVideo,
			Resolution:      resCode,
			ProfileCode:     profCode,
			FPS:             fps,
			TotalFrames:     stats.TotalFrames,
			ManifestFrames:  stats.ManifestFrames,
			VideoSizeBytes:  stats.VideoSizeBytes,
			DataSizeBytes:   manifest.TotalSize,
			FileCount:       len(manifest.Files),
			Status:          "staged",
			Privacy:         privacy,
			ContainerSalt:   contSalt,
			KeyScope:        "folder",
			FolderID:        diff.FolderID,
			FolderKeyHex:    fkHex,
			SyncID:          diff.SyncID,
			DeltaIndex:      deltaIdx,
			ParentVideoID:   manifest.ParentVideoID,
			DeletedRelPaths: manifest.DeletedRelPaths,
			CreatedAt:       time.Now().Format(time.RFC3339),
		}

		if req.AutoUploadYT {
			if tok, err := s.ensureYouTubeAccessToken(); err == nil && tok != "" {
				tUp := time.Now()
				s.updateJobETA(job, 88, "Enviando vídeo incremental para o YouTube...", "calculando...", "")
				vid, upErr := youtube.UploadVideoAPI(tok, outVideo, shortCode, "YTCloud Incremental Container "+diff.SyncID, privacy,
					func(sent, total int64) {
						pct := 88 + int((sent*10)/maxInt64(total, 1))
						el := time.Since(tUp)
						spd := ""
						if el > 200*time.Millisecond {
							spd = fmt.Sprintf("%.1f MB/s", float64(sent)/el.Seconds()/(1024*1024))
						}
						s.updateJobETA(job, pct, "Enviando para o YouTube...", "", spd)
					})
				if upErr == nil {
					container.YouTubeVideoID = vid
					container.YouTubeURL = "https://www.youtube.com/watch?v=" + vid
					container.Status = "uploaded"
					if settings.CleanupPolicy == "immediate_after_upload" {
						if os.Remove(outVideo) == nil {
							container.LocalDeleted = true
						}
					}
				}
			}
		}

		now := time.Now().Format(time.RFC3339)
		err = s.store.Update(func(d *vault.VaultData) error {
			if d.SyncGroups == nil {
				d.SyncGroups = map[string]*vault.SyncGroup{}
			}
			d.Containers[container.ID] = container

			// Map existing files in this folder by RelPath so modified files get updated in place (keeping user-edited metadata!)
			existingByRel := map[string]*vault.FileEntry{}
			for _, fe := range d.Files {
				existingByRel[fe.RelPathInVideo] = fe
			}

			if req.ApplyDeletes && len(diff.Deleted) > 0 {
				for _, del := range diff.Deleted {
					delete(d.Files, del.FileID)
				}
			}

			for _, pf := range manifest.Files {
				dirPart := filepath.Dir(pf.RelPath)
				targetFolder := diff.FolderID
				if dirPart != "." && dirPart != "/" && dirPart != "" {
					targetFolder = s.store.EnsureFolderPathLocked(d, diff.FolderID, dirPart)
				}
				if old, exists := existingByRel[pf.RelPath]; exists {
					// Update modified file pointing to the new Delta container while preserving DisplayName/EffectiveDate/Tags/Collections!
					old.ContainerID = container.ID
					old.SizeBytes = pf.OriginalSize
					old.SHA256 = pf.SHA256
					old.OriginalDate = pf.ModTime
					old.PreviewKind = pf.PreviewKind
					old.PreviewBase64 = pf.PreviewBase64
					old.LocalSourcePath = pf.LocalPath
					old.LocalFreed = false
					old.AddedAt = now
				} else {
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
			}

			sg := d.SyncGroups[diff.SyncID]
			if sg == nil {
				sg = &vault.SyncGroup{
					ID:              diff.SyncID,
					Name:            filepath.Base(diff.LocalPath),
					LocalPath:       diff.LocalPath,
					FolderID:        diff.FolderID,
					BaseContainerID: container.ID,
					MaxDeltas:       diff.MaxDeltas,
				}
				d.SyncGroups[diff.SyncID] = sg
			}
			if isBase {
				sg.BaseContainerID = container.ID
				sg.DeltaCount = 0
			} else {
				sg.DeltaCount = deltaIdx
			}
			sg.LastContainerID = container.ID
			sg.LastSyncedAt = now
			return nil
		})
		s.finishJob(job, container, err)
	}()

	writeJSON(w, job)
}

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ytcloud/internal/codec"
	"ytcloud/internal/crypto"
	"ytcloud/internal/media"
	"ytcloud/internal/packer"
	"ytcloud/internal/sysutil"
	"ytcloud/internal/vault"
	"ytcloud/internal/youtube"
)

// handleFileOpen handles native Windows file actions on a vault file:
//   - "open": opens the file using the OS default viewer/app (auto-extracts first if not on disk yet)
//   - "open_with": opens Windows "Abrir com..." dialog (auto-extracts first if not on disk yet)
//   - "reveal_local": opens Windows Explorer highlighting the last extracted local copy
//   - "reveal_origin": opens Windows Explorer highlighting the original source file/folder
//   - "open_container": opens the container's local video in Explorer or returns its YouTube URL
func (s *Server) handleFileOpen(w http.ResponseWriter, r *http.Request, fileID string) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		Mode string `json:"mode"` // "open", "open_with", "reveal_local", "reveal_origin", "open_container"
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "open"
	}

	var (
		fe   vault.FileEntry
		cont *vault.VideoContainer
	)
	err := s.store.Read(func(d *vault.VaultData) error {
		f, ok := d.Files[fileID]
		if !ok {
			return errors.New("arquivo não encontrado")
		}
		fe = *f
		if f.ContainerID != "" {
			if c, cok := d.Containers[f.ContainerID]; cok {
				cp := *c
				cont = &cp
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, 404, err.Error())
		return
	}

	findExistingLocal := func() string {
		if fe.LastExtractedPath != "" {
			if _, stErr := os.Stat(fe.LastExtractedPath); stErr == nil {
				return fe.LastExtractedPath
			}
		}
		if fe.LocalSourcePath != "" && !fe.LocalFreed {
			if _, stErr := os.Stat(fe.LocalSourcePath); stErr == nil {
				return fe.LocalSourcePath
			}
		}
		return ""
	}

	switch mode {
	case "reveal_local":
		p := ""
		if fe.LastExtractedPath != "" {
			if _, stErr := os.Stat(fe.LastExtractedPath); stErr == nil {
				p = fe.LastExtractedPath
			} else if _, stErr := os.Stat(filepath.Dir(fe.LastExtractedPath)); stErr == nil {
				p = filepath.Dir(fe.LastExtractedPath)
			}
		}
		if p == "" {
			p = findExistingLocal()
		}
		if p == "" {
			writeError(w, 400, "este arquivo ainda não foi baixado/extraído localmente")
			return
		}
		if err := sysutil.RevealInExplorer(p); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "path": p})
		return

	case "reveal_origin":
		if fe.LocalSourcePath == "" {
			writeError(w, 400, "caminho de origem não disponível para este arquivo")
			return
		}
		target := fe.LocalSourcePath
		if _, stErr := os.Stat(target); stErr != nil {
			dir := filepath.Dir(target)
			if _, dirErr := os.Stat(dir); dirErr == nil {
				target = dir
			} else {
				writeError(w, 400, "pasta de origem não encontrada neste PC: "+dir)
				return
			}
		}
		if err := sysutil.RevealInExplorer(target); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "path": target})
		return

	case "open_container":
		if cont == nil {
			writeError(w, 400, "este arquivo está desassociado (sem vídeo contêiner vinculado)")
			return
		}
		if cont.LocalVideoPath != "" && !cont.LocalDeleted {
			if _, stErr := os.Stat(cont.LocalVideoPath); stErr == nil {
				_ = sysutil.RevealInExplorer(cont.LocalVideoPath)
				writeJSON(w, map[string]any{"ok": true, "path": cont.LocalVideoPath, "container_id": cont.ID})
				return
			}
		}
		if cont.YouTubeURL != "" {
			_ = sysutil.OpenFileDefault(cont.YouTubeURL)
			writeJSON(w, map[string]any{"ok": true, "youtube_url": cont.YouTubeURL, "container_id": cont.ID})
			return
		}
		writeError(w, 400, "contêiner sem arquivo .mp4 local e sem link do YouTube")
		return
	}

	// mode == "open" or "open_with"
	if existing := findExistingLocal(); existing != "" {
		var openErr error
		if mode == "open_with" {
			openErr = sysutil.OpenFileWithDialog(existing)
		} else {
			openErr = sysutil.OpenFileDefault(existing)
		}
		if openErr != nil {
			writeError(w, 500, openErr.Error())
			return
		}
		writeJSON(w, map[string]any{"opened": true, "path": existing, "mode": mode})
		return
	}

	// Not on disk yet -> auto-extract from container in background and launch automatically!
	if cont == nil {
		writeError(w, 400, "arquivo não encontrado no disco e sem contêiner de vídeo vinculado")
		return
	}
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		writeError(w, 400, "FFmpeg necessário para extrair e abrir o arquivo")
		return
	}

	home, _ := os.UserHomeDir()
	destDir := filepath.Join(home, "Downloads", "BrightVault-Restaurados")
	job := s.createJob("open_file", fmt.Sprintf("Extraindo e abrindo %s", fe.DisplayName))
	contCopy := *cont

	go func() {
		dlDir := filepath.Join(s.stagingDir, "open_tmp", job.ID)
		defer os.RemoveAll(dlDir)

		videoPath := ""
		if contCopy.LocalVideoPath != "" && !contCopy.LocalDeleted {
			if _, stErr := os.Stat(contCopy.LocalVideoPath); stErr == nil {
				videoPath = contCopy.LocalVideoPath
			}
		}
		if videoPath == "" && contCopy.YouTubeURL != "" {
			s.updateJobETA(job, 10, fmt.Sprintf("Baixando vídeo %s do YouTube...", contCopy.ShortCode), "calculando...", "")
			dlOpts := s.ytDlpOpts()
			dlOpts.OnProgress = func(pct int, detail, speed, eta string) {
				s.updateJobETA(job, 10+(pct*35)/100, "Download: "+detail, eta, speed)
			}
			vp, dlErr := youtube.DownloadVideo(dlOpts, contCopy.YouTubeURL, dlDir)
			if dlErr != nil {
				s.finishJob(job, nil, dlErr)
				return
			}
			videoPath = vp
		}
		if videoPath == "" {
			s.finishJob(job, nil, fmt.Errorf("contêiner %s sem arquivo local e sem URL do YouTube", contCopy.ShortCode))
			return
		}

		s.updateJobETA(job, 48, "Decodificando frames ópticos + FEC...", "calculando...", "")
		decRes, decErr := codec.DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, videoPath, false, func(fr, tot int) {
			s.updateJobETA(job, 48+(fr*38)/maxInt(tot, 1), fmt.Sprintf("Decodificando frame %d/%d + FEC", fr, tot), "", "")
		})
		if decErr != nil {
			s.finishJob(job, nil, decErr)
			return
		}
		if len(decRes.EncryptedManifest) < 32 {
			s.finishJob(job, nil, errors.New("manifesto do vídeo corrompido"))
			return
		}
		salt := decRes.EncryptedManifest[:16]
		contUUID := string(decRes.EncryptedManifest[16:32])
		encManifest := decRes.EncryptedManifest[32:]

		contKey, kErr := s.store.ResolveContainerKey(&contCopy, salt)
		var manifestJSON []byte
		if kErr == nil {
			manifestJSON, kErr = crypto.DecryptAuto(contKey, encManifest, []byte("YTCloud-Manifest:"+contUUID))
			if kErr != nil {
				crypto.ZeroBytes(contKey)
				contKey = nil
			}
		}
		if contKey == nil {
			contKey, manifestJSON, kErr = s.store.TryDecryptVaultManifest(salt, contUUID, encManifest)
		}
		if kErr != nil {
			s.finishJob(job, nil, kErr)
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
			s.finishJob(job, nil, pErr)
			return
		}

		selRel := map[string]bool{fe.RelPathInVideo: true}
		dateOverrides := map[string]string{}
		if fe.EffectiveDate != "" {
			dateOverrides[fe.RelPathInVideo] = fe.EffectiveDate
		}
		s.updateJob(job, 90, "Extraindo arquivo...")
		extracted, exErr := packer.ExtractFiles(manifest, rawPayload, destDir, selRel, dateOverrides, tools.FFmpegPath)
		if exErr != nil || len(extracted) == 0 {
			if exErr == nil {
				exErr = errors.New("arquivo não encontrado dentro do vídeo")
			}
			s.finishJob(job, nil, exErr)
			return
		}
		outPath := extracted[0]
		_ = s.store.UpdateSystem(func(d *vault.VaultData) error {
			if f, ok := d.Files[fileID]; ok {
				f.LastExtractedPath = outPath
			}
			return nil
		})
		s.updateJob(job, 96, "Abrindo no aplicativo do Windows...")
		var openErr error
		if mode == "open_with" {
			openErr = sysutil.OpenFileWithDialog(outPath)
		} else {
			openErr = sysutil.OpenFileDefault(outPath)
		}
		if openErr != nil {
			s.finishJob(job, map[string]any{"path": outPath, "dest_dir": destDir}, openErr)
			return
		}
		s.finishJob(job, map[string]any{
			"opened":   true,
			"path":     outPath,
			"dest_dir": destDir,
			"file_id":  fileID,
			"mode":     mode,
			"at":       time.Now().Format(time.RFC3339),
		}, nil)
	}()

	writeJSON(w, map[string]any{"extracting": true, "job": job})
}

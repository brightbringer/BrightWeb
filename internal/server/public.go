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
	"ytcloud/internal/youtube"
)

// ytDlpOptsPublic returns yt-dlp options that work even before any vault is created or unlocked.
func (s *Server) ytDlpOptsPublic() youtube.YtDlpOptions {
	if s.store != nil && s.store.IsUnlocked() {
		return s.ytDlpOpts()
	}
	tools := media.DetectTools()
	return youtube.YtDlpOptions{
		YtDlpPath:  tools.YtDlpPath,
		FFmpegPath: tools.FFmpegPath,
	}
}

// handlePublicPeek inspects any Public Share video (YouTube URL or local file) using ONLY
// the video + 1..5 layered passwords — zero vault creation or unlock required!
func (s *Server) handlePublicPeek(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source    string   `json:"source"`
		Passwords []string `json:"passwords"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Source) == "" {
		writeError(w, 400, "informe o link do YouTube ou caminho de vídeo local")
		return
	}
	cleanPWs, err := crypto.NormalizePublicPasswords(req.Passwords)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		writeError(w, 400, "FFmpeg necessário para inspecionar vídeos")
		return
	}
	src := strings.Trim(strings.TrimSpace(req.Source), `"`)
	job := s.createJob("public_peek", "Inspecionando Vídeo Público (Link + Senha)")

	go func() {
		videoPath := src
		tempPeek := false
		if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || youtube.ExtractVideoID(src) != "" {
			if !tools.HasYtDlp {
				s.finishJob(job, nil, errors.New("yt-dlp necessário para baixar links do YouTube"))
				return
			}
			s.updateJobETA(job, 10, "Baixando apenas o trecho inicial do vídeo público...", "calculando...", "")
			dlOpts := s.ytDlpOptsPublic()
			dlOpts.OnProgress = func(pct int, detail, speed, eta string) {
				s.updateJobETA(job, 10+(pct*40)/100, "Baixando início: "+detail, eta, speed)
			}
			p, err := youtube.DownloadManifestPeek(dlOpts, src, filepath.Join(s.stagingDir, "public_peek"), 5)
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

		s.updateJobETA(job, 55, "Decodificando cabeçalho e manifesto óptico...", "alguns segundos...", "")
		decRes, err := codec.DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, videoPath, true, func(fr, tot int) {
			s.updateJobETA(job, 55+(fr*25)/maxInt(tot, 1), fmt.Sprintf("Lendo frame de manifesto %d...", fr), "quase lá...", "")
		})
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		isPub, layers, salt, contUUID, encManifest, err := crypto.ParsePublicManifest(decRes.EncryptedManifest)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		if !isPub {
			s.finishJob(job, nil, errors.New("este vídeo pertence a um cofre privado (não foi gerado em Modo Público)"))
			return
		}
		if len(cleanPWs) != layers {
			s.finishJob(job, nil, fmt.Errorf("este vídeo público exige exatamente %d senha(s) em cascata (você informou %d)", layers, len(cleanPWs)))
			return
		}

		s.updateJobETA(job, 88, fmt.Sprintf("Descriptografando cascata Pós-Quântica (%d camada(s) de senha)...", layers), "< 2s", "")
		manifestJSON, err := crypto.DecryptPublicCascade(cleanPWs, salt, encManifest, []byte("YTCloud-Public-Manifest:"+contUUID))
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		manifest, err := packer.UnmarshalManifest(manifestJSON)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		s.finishJob(job, map[string]any{
			"manifest":        manifest,
			"total_frames":    decRes.TotalFrames,
			"resolution":      fmt.Sprintf("%dx%d", decRes.Width, decRes.Height),
			"profile_code":    decRes.ProfileCode,
			"password_layers": layers,
			"is_public":       true,
		}, nil)
	}()

	writeJSON(w, job)
}

// handlePublicExtract downloads (if URL), decodes, decrypts with 1..5 layered passwords, and extracts
// files from any Public Share video without needing any vault unlocked or created!
func (s *Server) handlePublicExtract(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source    string   `json:"source"`
		Passwords []string `json:"passwords"`
		RelPaths  []string `json:"rel_paths"` // optional subset of files; empty = extract all
		DestDir   string   `json:"dest_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Source) == "" {
		writeError(w, 400, "informe o link do YouTube ou caminho de vídeo local")
		return
	}
	cleanPWs, err := crypto.NormalizePublicPasswords(req.Passwords)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	tools := media.DetectTools()
	if !tools.HasFFmpeg {
		writeError(w, 400, "FFmpeg necessário para extrair vídeos")
		return
	}
	src := strings.Trim(strings.TrimSpace(req.Source), `"`)
	destDir := strings.Trim(strings.TrimSpace(req.DestDir), `"`)
	if destDir == "" {
		home, _ := os.UserHomeDir()
		destDir = filepath.Join(home, "Downloads", "BrightVault-Publico")
	}

	job := s.createJob("public_extract", "Baixando e descriptografando Vídeo Público")
	go func() {
		videoPath := src
		tempDir := ""
		if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || youtube.ExtractVideoID(src) != "" {
			if !tools.HasYtDlp {
				s.finishJob(job, nil, errors.New("yt-dlp necessário para baixar links do YouTube"))
				return
			}
			dlID, _ := crypto.RandomHex(5)
			tempDir = filepath.Join(s.stagingDir, "public_dl", dlID)
			defer os.RemoveAll(tempDir)

			s.updateJobETA(job, 6, "Baixando vídeo público em resolução máxima...", "calculando...", "")
			dlOpts := s.ytDlpOptsPublic()
			dlOpts.OnProgress = func(pct int, detail, speed, eta string) {
				s.updateJobETA(job, 6+(pct*42)/100, "Baixando: "+detail, eta, speed)
			}
			p, err := youtube.DownloadVideo(dlOpts, src, tempDir)
			if err != nil {
				s.finishJob(job, nil, err)
				return
			}
			videoPath = p
		}

		decStart := time.Now()
		s.updateJobETA(job, 50, "Decodificando frames ópticos + Reed-Solomon FEC...", "calculando...", "")
		decRes, err := codec.DecodeVideoStream(tools.FFmpegPath, tools.FFprobePath, videoPath, false, func(fr, tot int) {
			pct := 50 + (fr*35)/maxInt(tot, 1)
			elapsed := time.Since(decStart)
			etaStr, spdStr := "calculando...", ""
			if elapsed > 200*time.Millisecond && fr > 0 {
				fpsCur := float64(fr) / elapsed.Seconds()
				spdStr = fmt.Sprintf("%.1f fps", fpsCur)
				if tot > fr {
					etaStr = fmtETA(time.Duration(float64(tot-fr)/fpsCur) * time.Second)
				}
			}
			s.updateJobETA(job, pct, fmt.Sprintf("Decodificando frame %d/%d + FEC", fr, tot), etaStr, spdStr)
		})
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		isPub, layers, salt, contUUID, encManifest, err := crypto.ParsePublicManifest(decRes.EncryptedManifest)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		if !isPub {
			s.finishJob(job, nil, errors.New("este vídeo pertence a um cofre privado (não foi gerado em Modo Público)"))
			return
		}
		if len(cleanPWs) != layers {
			s.finishJob(job, nil, fmt.Errorf("este vídeo público exige exatamente %d senha(s) em cascata (você informou %d)", layers, len(cleanPWs)))
			return
		}

		s.updateJobETA(job, 87, fmt.Sprintf("Descriptografando %d camada(s) Pós-Quânticas...", layers), "alguns segundos...", "")
		manifestJSON, err := crypto.DecryptPublicCascade(cleanPWs, salt, encManifest, []byte("YTCloud-Public-Manifest:"+contUUID))
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		manifest, err := packer.UnmarshalManifest(manifestJSON)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		rawPayload, err := crypto.DecryptPublicCascade(cleanPWs, salt, decRes.EncryptedPayload, []byte("YTCloud-Public-Payload:"+contUUID))
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}

		selectedRel := map[string]bool{}
		for _, rp := range req.RelPaths {
			if rp = strings.TrimSpace(rp); rp != "" {
				selectedRel[rp] = true
			}
		}

		s.updateJobETA(job, 94, "Extraindo arquivos para "+destDir+"...", "< 2s", "")
		extracted, err := packer.ExtractFiles(manifest, rawPayload, destDir, selectedRel, nil, tools.FFmpegPath)
		if err != nil {
			s.finishJob(job, nil, err)
			return
		}
		s.finishJob(job, map[string]any{
			"dest_dir":        destDir,
			"extracted":       extracted,
			"count":           len(extracted),
			"password_layers": layers,
		}, nil)
	}()

	writeJSON(w, job)
}

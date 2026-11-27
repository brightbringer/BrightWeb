package youtube

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ytcloud/internal/sysutil"
)

var ytIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?:v=|youtu\.be/|shorts/|embed/)([A-Za-z0-9_-]{11})`),
	regexp.MustCompile(`^([A-Za-z0-9_-]{11})$`),
}

// ExtractVideoID extracts the 11-character YouTube video ID from a URL or raw ID.
func ExtractVideoID(input string) string {
	input = strings.TrimSpace(input)
	for _, re := range ytIDPatterns {
		if m := re.FindStringSubmatch(input); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

// YtDlpOptions configures yt-dlp execution with JS runtime (unlocks 4K/8K formats),
// optional browser cookies (for Private videos), and real-time download progress reporting.
type YtDlpOptions struct {
	YtDlpPath      string
	FFmpegPath     string
	JSRuntime      string // e.g. "node:C:\Program Files\nodejs\node.exe"
	CookiesBrowser string // e.g. "edge", "chrome", "firefox", "brave"
	CookiesFile    string // optional path to Netscape cookies.txt
	OnProgress     func(pct int, detail, speed, eta string)
}

var (
	reDlPct   = regexp.MustCompile(`(\d+(?:\.\d+)?)%`)
	reDlSpeed = regexp.MustCompile(`at\s+([^\s]+/s)`)
	reDlETA   = regexp.MustCompile(`ETA\s+([0-9:]+)`)
)

func runYtDlpWithProgress(ytDlpPath string, args []string, onProgress func(pct int, detail, speed, eta string)) (string, error) {
	fullArgs := append([]string{"--newline", "--progress"}, args...)
	cmd := exec.Command(ytDlpPath, fullArgs...)
	sysutil.ApplySubprocessAttrs(cmd, true)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return "", err
	}

	var allOut strings.Builder
	doneRead := make(chan struct{})
	go func() {
		defer close(doneRead)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 512*1024)
		for sc.Scan() {
			line := sc.Text()
			allOut.WriteString(line)
			allOut.WriteByte('\n')
			if onProgress != nil && strings.Contains(line, "[download]") {
				if m := reDlPct.FindStringSubmatch(line); len(m) == 2 {
					pctF, _ := strconv.ParseFloat(m[1], 64)
					spd, eta := "", ""
					if sm := reDlSpeed.FindStringSubmatch(line); len(sm) == 2 {
						spd = sm[1]
					}
					if em := reDlETA.FindStringSubmatch(line); len(em) == 2 {
						eta = "~" + em[1] + " restantes"
					}
					onProgress(int(pctF), strings.TrimSpace(strings.TrimPrefix(line, "[download]")), spd, eta)
				}
			}
		}
	}()

	waitErr := cmd.Wait()
	_ = pw.Close()
	<-doneRead
	_ = pr.Close()
	return allOut.String(), waitErr
}

func appendYtDlpCommonArgs(args []string, opts YtDlpOptions) []string {
	if opts.FFmpegPath != "" {
		args = append(args, "--ffmpeg-location", filepath.Dir(opts.FFmpegPath))
	}
	if opts.JSRuntime != "" {
		args = append(args, "--js-runtimes", opts.JSRuntime)
	}
	if opts.CookiesFile != "" {
		args = append(args, "--cookies", opts.CookiesFile)
	} else if opts.CookiesBrowser != "" {
		args = append(args, "--cookies-from-browser", opts.CookiesBrowser)
	}
	return args
}

// DownloadVideo downloads the highest available video stream (8K/4K/1080p) using yt-dlp.
func DownloadVideo(opts YtDlpOptions, videoURL, destDir string) (string, error) {
	if opts.YtDlpPath == "" {
		return "", errors.New("yt-dlp não encontrado — clique em 'Instalar Ferramentas' nas configurações")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	vid := ExtractVideoID(videoURL)
	if vid == "" {
		vid = fmt.Sprintf("dl_%d", time.Now().Unix())
	}
	outTpl := filepath.Join(destDir, vid+".%(ext)s")
	args := []string{
		"--no-playlist",
		"--continue",
		"--part",
		"--concurrent-fragments", "8",
		"--http-chunk-size", "10M",
		"-f", "bestvideo/best",
		"-o", outTpl,
	}
	args = appendYtDlpCommonArgs(args, opts)
	args = append(args, videoURL)

	out, err := runYtDlpWithProgress(opts.YtDlpPath, args, opts.OnProgress)
	if err != nil {
		msg := strings.TrimSpace(out)
		if strings.Contains(msg, "Private video") {
			return "", errors.New("vídeo Privado no YouTube: clique em '🔓 Não Listado' no vídeo (continua 100% criptografado e oculto!) OU configure 'Cookies do Navegador' em Configurações → YouTube API")
		}
		return "", fmt.Errorf("yt-dlp falhou: %v — %s", err, msg)
	}
	matches, _ := filepath.Glob(filepath.Join(destDir, vid+".*"))
	for _, m := range matches {
		if !strings.HasSuffix(m, ".part") {
			return m, nil
		}
	}
	return "", errors.New("arquivo baixado pelo yt-dlp não encontrado")
}

// DirectStreamInfo holds the resolved direct media stream URL and video dimensions for zero-disk online streaming.
type DirectStreamInfo struct {
	StreamURL string
	Width     int
	Height    int
	Duration  float64
	UserAgent string
}

// ResolveDirectStreamURL uses yt-dlp to resolve the highest-quality direct video stream URL (1080p/2K/4K)
// without downloading the video to disk — enabling BrightWeb to "watch" and progressively decode sites live!
func ResolveDirectStreamURL(opts YtDlpOptions, videoURL string) (*DirectStreamInfo, error) {
	if opts.YtDlpPath == "" {
		return nil, errors.New("yt-dlp não encontrado")
	}
	args := []string{
		"--no-playlist",
		"--no-warnings",
		"-f", "bestvideo[protocol=https]/bestvideo/best",
		"--print", "%(url)s",
		"--print", "%(width)s",
		"--print", "%(height)s",
		"--print", "%(duration)s",
	}
	args = appendYtDlpCommonArgs(args, opts)
	args = append(args, videoURL)

	cmd := exec.Command(opts.YtDlpPath, args...)
	sysutil.ApplySubprocessAttrs(cmd, true)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("falha resolvendo stream direto: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 3 {
		return nil, errors.New("resposta incompleta do yt-dlp para stream direto")
	}
	streamURL := strings.TrimSpace(lines[0])
	if !strings.HasPrefix(streamURL, "http://") && !strings.HasPrefix(streamURL, "https://") {
		return nil, errors.New("URL de stream direto inválida")
	}
	w, _ := strconv.Atoi(strings.TrimSpace(lines[1]))
	h, _ := strconv.Atoi(strings.TrimSpace(lines[2]))
	var dur float64
	if len(lines) >= 4 {
		dur, _ = strconv.ParseFloat(strings.TrimSpace(lines[3]), 64)
	}
	return &DirectStreamInfo{
		StreamURL: streamURL,
		Width:     w,
		Height:    h,
		Duration:  dur,
	}, nil
}

// DownloadManifestPeek downloads ONLY the first `seconds` (default 5s) of a YouTube video
// using yt-dlp --download-sections so Manifest-First inspection works without downloading the full video!
func DownloadManifestPeek(opts YtDlpOptions, videoURL, destDir string, seconds int) (string, error) {
	if opts.YtDlpPath == "" {
		return "", errors.New("yt-dlp não encontrado")
	}
	if seconds <= 0 {
		seconds = 5
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	vid := ExtractVideoID(videoURL)
	if vid == "" {
		vid = fmt.Sprintf("peek_%d", time.Now().Unix())
	}
	outPath := filepath.Join(destDir, vid+"_peek.mp4")
	_ = os.Remove(outPath)

	args := []string{
		"--no-playlist",
		"-f", "bestvideo/best",
		"--download-sections", fmt.Sprintf("*0-%d", seconds),
		"--force-keyframes-at-cuts",
		"-o", outPath,
	}
	args = appendYtDlpCommonArgs(args, opts)
	args = append(args, videoURL)

	out, err := runYtDlpWithProgress(opts.YtDlpPath, args, opts.OnProgress)
	if err != nil {
		if strings.Contains(out, "Private video") {
			return "", errors.New("vídeo Privado no YouTube: clique em '🔓 Não Listado' no vídeo ou configure Cookies do Navegador em Configurações → YouTube API")
		}
		return DownloadVideo(opts, videoURL, destDir)
	}
	if _, err := os.Stat(outPath); err == nil {
		return outPath, nil
	}
	matches, _ := filepath.Glob(filepath.Join(destDir, vid+"_peek.*"))
	if len(matches) > 0 {
		return matches[0], nil
	}
	return "", fmt.Errorf("peek falhou: %s", strings.TrimSpace(out))
}

// CheckVideoAvailable checks if a YouTube video is online and what max height it has finished processing.
func CheckVideoAvailable(opts YtDlpOptions, videoURL string) (int, string, error) {
	if opts.YtDlpPath == "" {
		return 0, "", errors.New("yt-dlp não encontrado")
	}
	args := []string{"--no-playlist", "--dump-single-json", "--skip-download"}
	args = appendYtDlpCommonArgs(args, opts)
	args = append(args, videoURL)
	cmd := exec.Command(opts.YtDlpPath, args...)
	sysutil.ApplySubprocessAttrs(cmd, true)
	out, err := cmd.Output()
	if err != nil {
		return 0, "", errors.New("vídeo indisponível ou Privado (clique em '🔓 Não Listado' ou configure Cookies do Navegador em Configurações)")
	}
	var info struct {
		Title   string `json:"title"`
		Formats []struct {
			Height int `json:"height"`
		} `json:"formats"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, "", err
	}
	maxH := 0
	for _, f := range info.Formats {
		if f.Height > maxH {
			maxH = f.Height
		}
	}
	return maxH, info.Title, nil
}

// --- YouTube Data API v3 (OAuth2 Loopback + Resumable Upload + Catalog Video Listing & Deletion) ---

type OAuthTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiryUnix   int64  `json:"expiry_unix"`
	ChannelTitle string `json:"channel_title"`
}

// StartOAuthFlow opens a temporary local callback listener, returns the Google OAuth URL,
// and waits for the user to authorize YouTube access.
func StartOAuthFlow(ctx context.Context, clientID, clientSecret string, onAuthURL func(authURL string)) (*OAuthTokens, error) {
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("configure o YouTube Client ID e Client Secret nas configurações primeiro")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/oauth2callback", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if e := r.URL.Query().Get("error"); e != "" {
				errCh <- fmt.Errorf("OAuth recusado: %s", e)
				_, _ = w.Write([]byte("<h2>Autorização cancelada. Pode fechar esta aba.</h2>"))
				return
			}
			code := r.URL.Query().Get("code")
			if code == "" {
				return
			}
			codeCh <- code
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<body style='font-family:sans-serif;background:#090e1d;color:#f6efe0;padding:40px;text-align:center'><h2>☀️ BrightVault conectado ao YouTube!</h2><p>Você já pode fechar esta janela e voltar ao BrightVault.</p></body>"))
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "https://www.googleapis.com/auth/youtube https://www.googleapis.com/auth/youtube.upload")
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	authURL := "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode()

	if onAuthURL != nil {
		onAuthURL(authURL)
	}

	var code string
	select {
	case code = <-codeCh:
	case err = <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return exchangeToken(clientID, clientSecret, redirectURI, code)
}

func exchangeToken(clientID, clientSecret, redirectURI, code string) (*OAuthTokens, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")

	resp, err := http.PostForm("https://oauth2.googleapis.com/token", form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("erro OAuth token (%d): %s", resp.StatusCode, string(body))
	}
	var tok OAuthTokens
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, err
	}
	tok.ExpiryUnix = time.Now().Unix() + tok.ExpiresIn - 60
	tok.ChannelTitle, _ = fetchChannelTitle(tok.AccessToken)
	return &tok, nil
}

func RefreshAccessToken(clientID, clientSecret, refreshToken string) (*OAuthTokens, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")

	resp, err := http.PostForm("https://oauth2.googleapis.com/token", form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("erro renovando token YouTube (%d): %s", resp.StatusCode, string(body))
	}
	var tok OAuthTokens
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, err
	}
	tok.RefreshToken = refreshToken
	tok.ExpiryUnix = time.Now().Unix() + tok.ExpiresIn - 60
	return &tok, nil
}

func fetchChannelTitle(accessToken string) (string, error) {
	req, _ := http.NewRequest("GET", "https://www.googleapis.com/youtube/v3/channels?part=snippet&mine=true", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var parsed struct {
		Items []struct {
			Snippet struct {
				Title string `json:"title"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if json.NewDecoder(resp.Body).Decode(&parsed) == nil && len(parsed.Items) > 0 {
		return parsed.Items[0].Snippet.Title, nil
	}
	return "", nil
}

type progressReader struct {
	r          io.Reader
	total      int64
	sent       int64
	lastReport time.Time
	onProgress func(sent, total int64)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 {
		p.sent += int64(n)
		if p.onProgress != nil && (time.Since(p.lastReport) > 250*time.Millisecond || p.sent >= p.total) {
			p.lastReport = time.Now()
			p.onProgress(p.sent, p.total)
		}
	}
	return n, err
}

func videoMIMEType(videoPath string) string {
	switch strings.ToLower(filepath.Ext(videoPath)) {
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".avi":
		return "video/x-msvideo"
	default:
		return "video/mp4"
	}
}

// UploadVideoAPI uploads a video file to YouTube using the Resumable Upload API v3.
func UploadVideoAPI(accessToken, videoPath, title, description, privacy string, onProgress ...func(sent, total int64)) (string, error) {
	if privacy != "public" && privacy != "private" && privacy != "unlisted" {
		privacy = "unlisted"
	}
	mimeType := videoMIMEType(videoPath)
	fi, err := os.Stat(videoPath)
	if err != nil {
		return "", err
	}

	meta := map[string]any{
		"snippet": map[string]any{
			"title":       title,
			"description": description,
			"categoryId":  "28", // Science & Technology
		},
		"status": map[string]any{
			"privacyStatus":           privacy,
			"selfDeclaredMadeForKids": false,
		},
	}
	metaBytes, _ := json.Marshal(meta)

	initReq, _ := http.NewRequest("POST", "https://www.googleapis.com/upload/youtube/v3/videos?uploadType=resumable&part=snippet,status", bytes.NewReader(metaBytes))
	initReq.Header.Set("Authorization", "Bearer "+accessToken)
	initReq.Header.Set("Content-Type", "application/json; charset=UTF-8")
	initReq.Header.Set("X-Upload-Content-Length", fmt.Sprintf("%d", fi.Size()))
	initReq.Header.Set("X-Upload-Content-Type", mimeType)

	initResp, err := http.DefaultClient.Do(initReq)
	if err != nil {
		return "", err
	}
	defer initResp.Body.Close()
	if initResp.StatusCode != 200 {
		b, _ := io.ReadAll(initResp.Body)
		return "", fmt.Errorf("YouTube API init upload falhou (%d): %s", initResp.StatusCode, string(b))
	}
	uploadURL := initResp.Header.Get("Location")
	if uploadURL == "" {
		return "", errors.New("YouTube API não retornou Location de upload")
	}

	f, err := os.Open(videoPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var bodyReader io.Reader = f
	if len(onProgress) > 0 && onProgress[0] != nil {
		bodyReader = &progressReader{r: f, total: fi.Size(), onProgress: onProgress[0]}
	}

	upReq, _ := http.NewRequest("PUT", uploadURL, bodyReader)
	upReq.ContentLength = fi.Size()
	upReq.Header.Set("Authorization", "Bearer "+accessToken)
	upReq.Header.Set("Content-Type", mimeType)

	upResp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		return "", err
	}
	defer upResp.Body.Close()
	body, _ := io.ReadAll(upResp.Body)
	if upResp.StatusCode != 200 && upResp.StatusCode != 201 {
		return "", fmt.Errorf("YouTube API upload falhou (%d): %s", upResp.StatusCode, string(body))
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.ID == "" {
		return "", fmt.Errorf("resposta inesperada da YouTube API: %s", string(body))
	}
	return res.ID, nil
}

// SetVideoPrivacyAPI updates a YouTube video's privacyStatus ("public", "unlisted" or "private") via API v3.
func SetVideoPrivacyAPI(accessToken, videoID, privacy string) error {
	if videoID == "" {
		return errors.New("videoID vazio")
	}
	if privacy != "public" && privacy != "unlisted" && privacy != "private" {
		privacy = "unlisted"
	}
	payload, _ := json.Marshal(map[string]any{
		"id": videoID,
		"status": map[string]any{
			"privacyStatus": privacy,
		},
	})
	req, _ := http.NewRequest("PUT", "https://www.googleapis.com/youtube/v3/videos?part=status", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("erro alterando privacidade (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

// SetVideoTitleAPI updates a YouTube video's title via API v3.
func SetVideoTitleAPI(accessToken, videoID, title string) error {
	if videoID == "" {
		return errors.New("videoID vazio")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("título vazio")
	}
	payload, _ := json.Marshal(map[string]any{
		"id": videoID,
		"snippet": map[string]any{
			"title":      title,
			"categoryId": "28",
		},
	})
	req, _ := http.NewRequest("PUT", "https://www.googleapis.com/youtube/v3/videos?part=snippet", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("erro alterando título no YouTube (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

// DeleteVideoAPI deletes a YouTube video (used during atomic catalog rotation AFTER the new Video-Index is confirmed).
func DeleteVideoAPI(accessToken, videoID string) error {
	if videoID == "" {
		return nil
	}
	req, _ := http.NewRequest("DELETE", "https://www.googleapis.com/youtube/v3/videos?id="+url.QueryEscape(videoID), nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("erro deletando vídeo antigo %s (%d): %s", videoID, resp.StatusCode, string(b))
	}
	return nil
}

// CatalogVideoItem represents a YTC-CATALOG-* video discovered on the user's YouTube channel.
type CatalogVideoItem struct {
	VideoID     string `json:"video_id"`
	Title       string `json:"title"`
	PublishedAt string `json:"published_at"`
}

// FindCatalogVideosAPI searches the authenticated user's uploaded playlist for YTC-CATALOG-* videos.
func FindCatalogVideosAPI(accessToken string) ([]CatalogVideoItem, error) {
	// 1. Get uploads playlist ID
	req, _ := http.NewRequest("GET", "https://www.googleapis.com/youtube/v3/channels?part=contentDetails&mine=true", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ch struct {
		Items []struct {
			ContentDetails struct {
				RelatedPlaylists struct {
					Uploads string `json:"uploads"`
				} `json:"relatedPlaylists"`
			} `json:"contentDetails"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil || len(ch.Items) == 0 {
		return nil, errors.New("não foi possível localizar canal do YouTube")
	}
	uploadsID := ch.Items[0].ContentDetails.RelatedPlaylists.Uploads

	// 2. List recent uploaded videos
	req2, _ := http.NewRequest("GET", "https://www.googleapis.com/youtube/v3/playlistItems?part=snippet&maxResults=50&playlistId="+url.QueryEscape(uploadsID), nil)
	req2.Header.Set("Authorization", "Bearer "+accessToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	var pl struct {
		Items []struct {
			Snippet struct {
				Title       string `json:"title"`
				PublishedAt string `json:"publishedAt"`
				ResourceID  struct {
					VideoID string `json:"videoId"`
				} `json:"resourceId"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&pl); err != nil {
		return nil, err
	}
	var out []CatalogVideoItem
	for _, it := range pl.Items {
		if strings.HasPrefix(it.Snippet.Title, "BV-CATALOG-") || strings.HasPrefix(it.Snippet.Title, "YTC-CATALOG-") {
			out = append(out, CatalogVideoItem{
				VideoID:     it.Snippet.ResourceID.VideoID,
				Title:       it.Snippet.Title,
				PublishedAt: it.Snippet.PublishedAt,
			})
		}
	}
	return out, nil
}

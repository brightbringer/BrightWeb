package media

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type ToolStatus struct {
	FFmpegPath  string `json:"ffmpeg_path"`
	FFprobePath string `json:"ffprobe_path"`
	YtDlpPath   string `json:"yt_dlp_path"`
	JSRuntime   string `json:"js_runtime"`
	HasFFmpeg   bool   `json:"has_ffmpeg"`
	HasYtDlp    bool   `json:"has_yt_dlp"`
	HasJS       bool   `json:"has_js_runtime"`
	BinDir      string `json:"bin_dir"`
}

func DefaultAppDir() string {
	if localApp := os.Getenv("LOCALAPPDATA"); localApp != "" {
		return filepath.Join(localApp, "YTCloud")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ytcloud")
}

func DefaultBinDir() string {
	return filepath.Join(DefaultAppDir(), "bin")
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func findBinary(name string) string {
	binDir := DefaultBinDir()
	// 1. Check alongside YTCloud.exe
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), exeName(name))
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		cand2 := filepath.Join(filepath.Dir(self), "bin", exeName(name))
		if _, err := os.Stat(cand2); err == nil {
			return cand2
		}
	}
	// 2. Check %LOCALAPPDATA%\YTCloud\bin
	cand := filepath.Join(binDir, exeName(name))
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	// 3. Check system PATH
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func detectJSRuntime() string {
	if p := findBinary("deno"); p != "" {
		return "deno:" + p
	}
	if p := findBinary("node"); p != "" {
		return "node:" + p
	}
	if runtime.GOOS == "windows" {
		for _, cand := range []string{
			filepath.Join(os.Getenv("ProgramFiles"), "nodejs", "node.exe"),
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "nodejs", "node.exe"),
		} {
			if _, err := os.Stat(cand); err == nil {
				return "node:" + cand
			}
		}
	}
	return ""
}

func DetectTools() ToolStatus {
	ff := findBinary("ffmpeg")
	fp := findBinary("ffprobe")
	yt := findBinary("yt-dlp")
	js := detectJSRuntime()
	return ToolStatus{
		FFmpegPath:  ff,
		FFprobePath: fp,
		YtDlpPath:   yt,
		JSRuntime:   js,
		HasFFmpeg:   ff != "",
		HasYtDlp:    yt != "",
		HasJS:       js != "",
		BinDir:      DefaultBinDir(),
	}
}

// InstallYtDlp downloads the official yt-dlp release binary into %LOCALAPPDATA%\YTCloud\bin.
func InstallYtDlp(onStatus func(msg string)) (string, error) {
	binDir := DefaultBinDir()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	url := "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe"
	if runtime.GOOS != "windows" {
		url = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp"
	}
	if onStatus != nil {
		onStatus("Baixando yt-dlp oficial...")
	}
	dst := filepath.Join(binDir, exeName("yt-dlp"))
	if err := downloadFile(url, dst); err != nil {
		return "", fmt.Errorf("erro baixando yt-dlp: %w", err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dst, 0o755)
	}
	return dst, nil
}

// InstallFFmpeg downloads the official portable FFmpeg essentials build (Windows) into %LOCALAPPDATA%\YTCloud\bin.
func InstallFFmpeg(onStatus func(msg string)) (string, string, error) {
	if runtime.GOOS != "windows" {
		return "", "", fmt.Errorf("instale ffmpeg pelo gerenciador de pacotes do sistema")
	}
	binDir := DefaultBinDir()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", "", err
	}
	zipPath := filepath.Join(binDir, "ffmpeg-essentials.zip")
	defer os.Remove(zipPath)

	url := "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"
	if onStatus != nil {
		onStatus("Baixando FFmpeg portátil oficial (~85 MB)...")
	}
	if err := downloadFile(url, zipPath); err != nil {
		return "", "", fmt.Errorf("erro baixando FFmpeg: %w", err)
	}

	if onStatus != nil {
		onStatus("Extraindo ffmpeg.exe e ffprobe.exe...")
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", "", err
	}
	defer zr.Close()

	var ffPath, fpPath string
	for _, f := range zr.File {
		base := strings.ToLower(filepath.Base(f.Name))
		if base == "ffmpeg.exe" || base == "ffprobe.exe" {
			dst := filepath.Join(binDir, base)
			rc, err := f.Open()
			if err != nil {
				return "", "", err
			}
			out, err := os.Create(dst)
			if err != nil {
				rc.Close()
				return "", "", err
			}
			_, copyErr := io.Copy(out, rc)
			out.Close()
			rc.Close()
			if copyErr != nil {
				return "", "", copyErr
			}
			if base == "ffmpeg.exe" {
				ffPath = dst
			} else {
				fpPath = dst
			}
		}
	}
	if ffPath == "" {
		return "", "", fmt.Errorf("ffmpeg.exe não encontrado no arquivo baixado")
	}
	return ffPath, fpPath, nil
}

func downloadFile(url, dst string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d ao baixar %s", resp.StatusCode, url)
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

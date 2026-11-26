package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ytcloud/internal/browser"
	"ytcloud/internal/media"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "Endereço local do gateway BrightWeb")
	noWindow := flag.Bool("no-window", false, "Não abrir a janela automaticamente")
	flag.Parse()

	initialTarget := ""
	if flag.NArg() > 0 {
		initialTarget = flag.Arg(0)
	}

	srv, err := browser.New(initialTarget)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Erro inicializando BrightWeb: %v\n", err)
		os.Exit(1)
	}

	ln, appURL, err := srv.Start(*addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Erro iniciando servidor BrightWeb: %v\n", err)
		os.Exit(1)
	}
	defer ln.Close()

	_ = os.MkdirAll(media.DefaultAppDir(), 0o755)
	logLine := fmt.Sprintf("[%s] BrightWeb iniciado | URL: %s\n", time.Now().Format(time.RFC3339), appURL)
	_ = os.WriteFile(filepath.Join(media.DefaultAppDir(), "brightweb.log"), []byte(logLine), 0o600)

	if !*noWindow {
		browser.LaunchBrowserWindow(appURL)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case <-srv.ShutdownChan():
	}
}

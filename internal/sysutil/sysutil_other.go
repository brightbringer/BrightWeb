//go:build !windows

package sysutil

import (
	"errors"
	"os/exec"
	"time"
)

func ApplySubprocessAttrs(cmd *exec.Cmd, lowPriority bool)      {}
func SetSelfBackgroundPriority(lowPriority bool)                {}
func MaximizeWindowByTitle(titleSubstr string, d time.Duration) {}
func TotalSystemRAMMB() int                                     { return 8192 }

func DPAPIProtect(plain []byte) ([]byte, error) {
	return nil, errors.New("DPAPI disponível apenas no Windows")
}

func DPAPIUnprotect(cipher []byte) ([]byte, error) {
	return nil, errors.New("DPAPI disponível apenas no Windows")
}

func PickFolderDialog(title string) (string, error) {
	return "", errors.New("seletor nativo disponível no Windows")
}

func PickFilesDialog(title string) ([]string, error) {
	return nil, errors.New("seletor nativo disponível no Windows")
}

func OpenFileDefault(path string) error {
	return exec.Command("xdg-open", path).Start()
}

func OpenFileWithDialog(path string) error {
	return exec.Command("xdg-open", path).Start()
}

func RevealInExplorer(path string) error {
	return exec.Command("xdg-open", path).Start()
}

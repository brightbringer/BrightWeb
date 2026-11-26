//go:build windows

package sysutil

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

const (
	createNoWindow           uint32 = 0x08000000
	belowNormalPriorityClass uint32 = 0x00004000
	normalPriorityClass      uint32 = 0x00000020

	ofnAllowMultiSelect uint32 = 0x00000200
	ofnNoChangeDir      uint32 = 0x00000008
	ofnFileMustExist    uint32 = 0x00001000
	ofnExplorer         uint32 = 0x00080000

	bifReturnOnlyFSDirs uint32 = 0x00000001
	bifNewDialogStyle   uint32 = 0x00000040
	bifEditBox          uint32 = 0x00000010

	coinitApartmentThreaded uintptr = 0x2
)

var (
	modKernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentProc       = modKernel32.NewProc("GetCurrentProcess")
	procSetPriorityClass     = modKernel32.NewProc("SetPriorityClass")
	procLocalFree            = modKernel32.NewProc("LocalFree")
	procGlobalMemoryStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")

	modCrypt32           = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData = modCrypt32.NewProc("CryptProtectData")
	procCryptUnprotect   = modCrypt32.NewProc("CryptUnprotectData")

	modUser32               = syscall.NewLazyDLL("user32.dll")
	procGetForegroundWindow = modUser32.NewProc("GetForegroundWindow")
	procEnumWindows         = modUser32.NewProc("EnumWindows")
	procIsWindowVisible     = modUser32.NewProc("IsWindowVisible")
	procGetWindowTextW      = modUser32.NewProc("GetWindowTextW")
	procShowWindow          = modUser32.NewProc("ShowWindow")
	procSetForegroundWindow = modUser32.NewProc("SetForegroundWindow")

	modComdlg32          = syscall.NewLazyDLL("comdlg32.dll")
	procGetOpenFileNameW = modComdlg32.NewProc("GetOpenFileNameW")

	modShell32              = syscall.NewLazyDLL("shell32.dll")
	procSHBrowseForFolderW  = modShell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDList = modShell32.NewProc("SHGetPathFromIDListW")
	procShellExecuteW       = modShell32.NewProc("ShellExecuteW")

	modOle32           = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx = modOle32.NewProc("CoInitializeEx")
	procCoUninitialize = modOle32.NewProc("CoUninitialize")
	procCoTaskMemFree  = modOle32.NewProc("CoTaskMemFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

type openFileNameW struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        unsafe.Pointer
	dwReserved        uint32
	flagsEx           uint32
}

type browseInfoW struct {
	hwndOwner      uintptr
	pidlRoot       uintptr
	pszDisplayName *uint16
	lpszTitle      *uint16
	ulFlags        uint32
	lpfn           uintptr
	lParam         uintptr
	iImage         int32
}

// ApplySubprocessAttrs hides the console window (CREATE_NO_WINDOW) and optionally runs in background priority.
func ApplySubprocessAttrs(cmd *exec.Cmd, lowPriority bool) {
	flags := createNoWindow
	if lowPriority {
		flags |= belowNormalPriorityClass
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: flags,
	}
}

// SetSelfBackgroundPriority adjusts the current YTCloud process priority on Windows.
func SetSelfBackgroundPriority(lowPriority bool) {
	h, _, _ := procGetCurrentProc.Call()
	if h == 0 {
		return
	}
	cls := uintptr(normalPriorityClass)
	if lowPriority {
		cls = uintptr(belowNormalPriorityClass)
	}
	_, _, _ = procSetPriorityClass.Call(h, cls)
}

type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

// TotalSystemRAMMB returns total installed physical RAM on Windows in megabytes (fallback 8192 MB).
func TotalSystemRAMMB() int {
	var ms memoryStatusEx
	ms.dwLength = uint32(unsafe.Sizeof(ms))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 || ms.ullTotalPhys == 0 {
		return 8192
	}
	mb := int(ms.ullTotalPhys / (1024 * 1024))
	if mb < 1024 {
		return 1024
	}
	return mb
}

// DPAPIProtect encrypts bytes bound strictly to the current Windows user account.
func DPAPIProtect(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, errors.New("dados vazios")
	}
	inBlob := dataBlob{cbData: uint32(len(plain)), pbData: &plain[0]}
	var outBlob dataBlob
	r, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, 0, 0, 0,
		0x01, // CRYPTPROTECT_UI_FORBIDDEN
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))
	out := make([]byte, outBlob.cbData)
	copy(out, unsafe.Slice(outBlob.pbData, outBlob.cbData))
	return out, nil
}

// DPAPIUnprotect decrypts bytes previously encrypted with DPAPIProtect for this Windows user.
func DPAPIUnprotect(cipher []byte) ([]byte, error) {
	if len(cipher) == 0 {
		return nil, errors.New("blob vazio")
	}
	inBlob := dataBlob{cbData: uint32(len(cipher)), pbData: &cipher[0]}
	var outBlob dataBlob
	r, _, err := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, 0, 0, 0,
		0x01, // CRYPTPROTECT_UI_FORBIDDEN
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))
	out := make([]byte, outBlob.cbData)
	copy(out, unsafe.Slice(outBlob.pbData, outBlob.cbData))
	return out, nil
}

// PickFolderDialog opens a native Win32 folder selection dialog directly in front of the active window
// (pure Go shell32.dll — instant, zero PowerShell, never hides behind the app!).
func PickFolderDialog(title string) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, _, _ := procCoInitializeEx.Call(0, coinitApartmentThreaded)
	if hr == 0 || hr == 1 {
		defer procCoUninitialize.Call()
	}

	hwnd, _, _ := procGetForegroundWindow.Call()
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	displayBuf := make([]uint16, 4096)

	bi := browseInfoW{
		hwndOwner:      hwnd,
		pszDisplayName: &displayBuf[0],
		lpszTitle:      titlePtr,
		ulFlags:        bifReturnOnlyFSDirs | bifNewDialogStyle | bifEditBox,
	}

	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return "", nil // User cancelled
	}
	defer procCoTaskMemFree.Call(pidl)

	pathBuf := make([]uint16, 4096)
	ok, _, _ := procSHGetPathFromIDList.Call(pidl, uintptr(unsafe.Pointer(&pathBuf[0])))
	if ok == 0 {
		return "", errors.New("não foi possível obter o caminho da pasta selecionada")
	}
	return syscall.UTF16ToString(pathBuf), nil
}

// PickFilesDialog opens the native Win32 Open File dialog (comdlg32.dll) directly in front of the active window.
func PickFilesDialog(title string) ([]string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hwnd, _, _ := procGetForegroundWindow.Call()
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	filter := utf16.Encode([]rune("Todos os Arquivos (*.*)\x00*.*\x00\x00"))
	fileBuf := make([]uint16, 65536) // supports hundreds of selected files at once

	ofn := openFileNameW{
		hwndOwner:   hwnd,
		lpstrFilter: &filter[0],
		lpstrFile:   &fileBuf[0],
		nMaxFile:    uint32(len(fileBuf)),
		lpstrTitle:  titlePtr,
		flags:       ofnExplorer | ofnAllowMultiSelect | ofnFileMustExist | ofnNoChangeDir,
	}
	ofn.lStructSize = uint32(unsafe.Sizeof(ofn))

	ret, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if ret == 0 {
		return nil, nil // User cancelled
	}

	// Parse NUL-delimited UTF-16 list:
	// Single file:  "C:\dir\file.jpg\0\0"
	// Multi files:  "C:\dir\0file1.jpg\0file2.jpg\0\0"
	var parts []string
	start := 0
	for i := 0; i < len(fileBuf); i++ {
		if fileBuf[i] == 0 {
			if i == start {
				break
			}
			parts = append(parts, string(utf16.Decode(fileBuf[start:i])))
			start = i + 1
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	if len(parts) == 1 {
		return parts, nil
	}
	dir := parts[0]
	out := make([]string, 0, len(parts)-1)
	for _, name := range parts[1:] {
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}

// MaximizeWindowByTitle polls briefly for a visible top-level window whose title contains titleSubstr
// and maximizes it via Win32 ShowWindow(SW_MAXIMIZE), ensuring Edge/Chrome --app windows start maximized
// even when an existing browser process ignores --start-maximized.
func MaximizeWindowByTitle(titleSubstr string, timeout time.Duration) {
	if titleSubstr == "" {
		return
	}
	const swMaximize = 3
	deadline := time.Now().Add(timeout)
	buf := make([]uint16, 256)
	var foundHwnd uintptr

	cb := syscall.NewCallback(func(hwnd, lparam uintptr) uintptr {
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		if vis == 0 {
			return 1
		}
		n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n > 0 {
			title := syscall.UTF16ToString(buf[:n])
			if strings.Contains(title, titleSubstr) {
				foundHwnd = hwnd
				return 0 // stop enumeration
			}
		}
		return 1
	})

	for time.Now().Before(deadline) {
		foundHwnd = 0
		_, _, _ = procEnumWindows.Call(cb, 0)
		if foundHwnd != 0 {
			_, _, _ = procShowWindow.Call(foundHwnd, swMaximize)
			_, _, _ = procSetForegroundWindow.Call(foundHwnd)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// OpenFileDefault launches any file or URL using the default Windows application via ShellExecuteW.
func OpenFileDefault(target string) error {
	verbPtr, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	filePtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	var dirPtr *uint16
	if !strings.HasPrefix(strings.ToLower(target), "http://") && !strings.HasPrefix(strings.ToLower(target), "https://") {
		dirPtr, _ = syscall.UTF16PtrFromString(filepath.Dir(target))
	}
	const swShowNormal = 1
	r, _, _ := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verbPtr)),
		uintptr(unsafe.Pointer(filePtr)),
		0,
		uintptr(unsafe.Pointer(dirPtr)),
		swShowNormal,
	)
	if r <= 32 {
		// Fallback via rundll32 url.dll
		return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", target).Start()
	}
	return nil
}

// OpenFileWithDialog opens the native Windows "Abrir com..." ("Open With") application picker dialog.
func OpenFileWithDialog(filePath string) error {
	cmd := exec.Command("rundll32.exe", "shell32.dll,OpenAs_RunDLL", filePath)
	return cmd.Start()
}

// RevealInExplorer opens Windows Explorer highlighting filePath (or opening it directly if it's a directory).
func RevealInExplorer(targetPath string) error {
	return exec.Command("explorer.exe", "/select,"+targetPath).Start()
}

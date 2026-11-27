package vault

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ytcloud/internal/crypto"
	"ytcloud/internal/packer"
	"ytcloud/internal/sysutil"

	"github.com/klauspost/compress/zstd"
)

const (
	VaultFileMagic = "YTV1"
	HeaderMagicStr = "YTCLOUD_VAULT_V1"
)

type Store struct {
	mu       sync.RWMutex
	path     string
	header   *VaultHeader
	data     *VaultData
	dek      []byte
	unlocked bool
}

func NewStore(vaultPath string) *Store {
	return &Store{path: vaultPath}
}

func (s *Store) Path() string { return s.path }

func (s *Store) RememberFilePath() string {
	return filepath.Join(filepath.Dir(s.path), "remember.dpapi")
}

func (s *Store) HasRememberedPassword() bool {
	_, err := os.Stat(s.RememberFilePath())
	return err == nil
}

type rememberedCreds struct {
	Password    string `json:"password"`
	KeyfilePath string `json:"keyfile_path,omitempty"`
}

func (s *Store) SaveRememberedPassword(password string) error {
	return s.SaveRememberedCreds(password, "")
}

func (s *Store) SaveRememberedCreds(password, keyfilePath string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(rememberedCreds{Password: password, KeyfilePath: keyfilePath})
	if err != nil {
		return err
	}
	defer crypto.ZeroBytes(raw)
	blob, err := sysutil.DPAPIProtect(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(s.RememberFilePath(), blob, 0o600)
}

func (s *Store) LoadRememberedPassword() (string, error) {
	pass, _, err := s.LoadRememberedCreds()
	return pass, err
}

func (s *Store) LoadRememberedCreds() (string, string, error) {
	blob, err := os.ReadFile(s.RememberFilePath())
	if err != nil {
		return "", "", err
	}
	plain, err := sysutil.DPAPIUnprotect(blob)
	if err != nil {
		return "", "", err
	}
	defer crypto.ZeroBytes(plain)
	var c rememberedCreds
	if json.Unmarshal(plain, &c) == nil && c.Password != "" {
		return c.Password, c.KeyfilePath, nil
	}
	// Legacy plain-string DPAPI blob
	return string(plain), "", nil
}

// InspectHeader reads only the unencrypted VaultHeader (safe before unlock) to check Keyfile requirements.
func (s *Store) InspectHeader() (*VaultHeader, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	hdr, _, err := parseVaultFile(raw)
	return hdr, err
}

// KeyfileInfo returns whether the currently unlocked vault uses a local Keyfile and its filename hint.
func (s *Store) KeyfileInfo() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.header == nil || s.header.KeyBundle == nil {
		return false, ""
	}
	return s.header.KeyBundle.UsesKeyfile, s.header.KeyBundle.KeyfileHint
}

func (s *Store) ClearRememberedPassword() error {
	err := os.Remove(s.RememberFilePath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

func (s *Store) IsUnlocked() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.unlocked
}

func (s *Store) Lock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dek != nil {
		crypto.ZeroBytes(s.dek)
		s.dek = nil
	}
	s.data = nil
	s.unlocked = false
}

// Create initializes a brand-new encrypted vault on disk.
func (s *Store) Create(vaultName, password string) error {
	return s.CreateWithKeyfile(vaultName, password, nil, "")
}

// CreateWithKeyfile initializes a brand-new encrypted vault with password + optional local Keyfile (PQ cascade).
func (s *Store) CreateWithKeyfile(vaultName, password string, keyfileBytes []byte, keyfileHint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if vaultName == "" {
		vaultName = "Meu Cofre"
	}
	bundle, dek, err := crypto.CreateKeyBundleWithKeyfile(password, keyfileBytes, keyfileHint)
	if err != nil {
		return err
	}
	vid, err := crypto.RandomHex(8)
	if err != nil {
		crypto.ZeroBytes(dek)
		return err
	}
	now := time.Now().Format(time.RFC3339)

	rootKey, _ := crypto.RandomHex(32)
	rootFolder := &Folder{
		ID:           "root",
		ParentID:     "",
		Name:         "Meu BrightVault",
		FolderKeyHex: rootKey,
		CreatedAt:    now,
	}

	s.header = &VaultHeader{
		Magic:     HeaderMagicStr,
		VaultID:   vid,
		VaultName: vaultName,
		KeyBundle: bundle,
	}
	defaultMaxDeltas := 10
	s.data = &VaultData{
		Version:         1,
		SettingsVersion: 3,
		VaultID:         vid,
		VaultName:       vaultName,
		Settings: VaultSettings{
			DefaultResolution:    "4k",
			DefaultProfile:       "y8-gray",
			DefaultFPS:           10,
			MaxRAMMB:             512,
			CPUThreads:           2,
			BackgroundMode:       true,
			CleanupPolicy:        "safe_after_verify",
			CatalogKeepPrevious:  false,
			CatalogIncludeThumbs: true,
			MaxDeltaVideos:       &defaultMaxDeltas,
			DefaultPrivacy:       "unlisted",
		},
		Folders:          map[string]*Folder{"root": rootFolder},
		Collections:      map[string]*Collection{},
		Files:            map[string]*FileEntry{},
		Containers:       map[string]*VideoContainer{},
		SyncGroups:       map[string]*SyncGroup{},
		PendingDownloads: map[string]*PendingDownload{},
	}
	s.dek = dek
	s.unlocked = true
	return s.saveLocked()
}

// Unlock opens an existing vault.ytdb file using the master password.
func (s *Store) Unlock(password string) error {
	return s.UnlockWithKeyfile(password, nil)
}

// UnlockWithKeyfile opens an existing vault.ytdb file using password + optional local Keyfile.
func (s *Store) UnlockWithKeyfile(password string, keyfileBytes []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	hdr, encBody, err := parseVaultFile(raw)
	if err != nil {
		return err
	}
	dek, err := crypto.UnlockKeyBundleWithKeyfile(hdr.KeyBundle, password, keyfileBytes)
	if err != nil {
		return err
	}
	data, err := decryptVaultBody(dek, hdr.VaultID, encBody)
	if err != nil {
		crypto.ZeroBytes(dek)
		return err
	}
	s.header = hdr
	s.data = data
	s.dek = dek
	s.unlocked = true
	changed := EnsureFolderKeysLocked(s.data)
	if s.data.VaultName == "Meu Cofre YTCloud" || s.data.VaultName == "Meu Cofre BrightVault" {
		s.data.VaultName = "Meu Cofre"
		if s.header != nil {
			s.header.VaultName = "Meu Cofre"
		}
		changed = true
	}
	if changed {
		_ = s.saveLocked()
	}
	return nil
}

// ChangePassword re-wraps the DEK without re-encrypting any YouTube videos.
func (s *Store) ChangePassword(currentPassword, newPassword string) error {
	return s.ChangeAuth(currentPassword, nil, newPassword, nil, "", "")
}

// ChangeAuth updates master password and/or local Keyfile without re-encrypting any YouTube videos!
func (s *Store) ChangeAuth(currentPassword string, currentKeyfile []byte, newPassword string, newKeyfile []byte, newKeyfileHint, newKeyfilePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.unlocked {
		return errors.New("cofre bloqueado")
	}
	// Verify current password + current keyfile first
	testDEK, err := crypto.UnlockKeyBundleWithKeyfile(s.header.KeyBundle, currentPassword, currentKeyfile)
	if err != nil {
		return errors.New("senha atual ou arquivo-chave atual incorreto")
	}
	crypto.ZeroBytes(testDEK)

	if newPassword == "" {
		newPassword = currentPassword
	}
	newBundle, err := crypto.RewrapKeyBundleWithKeyfile(s.dek, newPassword, newKeyfile, newKeyfileHint)
	if err != nil {
		return err
	}
	s.header.KeyBundle = newBundle
	if err := s.saveLocked(); err != nil {
		return err
	}
	if s.HasRememberedPassword() {
		_ = s.SaveRememberedCreds(newPassword, newKeyfilePath)
	}
	return nil
}

// DeriveContainerKey derives an independent AES-256 key for a specific legacy video container using the Vault Master DEK.
func (s *Store) DeriveContainerKey(containerSalt []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.unlocked {
		return nil, errors.New("cofre bloqueado")
	}
	return crypto.DeriveSubkey(s.dek, containerSalt, "YTCloud-VideoContainer-v1")
}

// DeriveFolderContainerKey derives an independent AES-256 container key from a folder's 32-byte key hex and container salt.
func DeriveFolderContainerKey(folderKeyHex string, containerSalt []byte) ([]byte, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(folderKeyHex))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("chave da pasta inválida")
	}
	defer crypto.ZeroBytes(raw)
	return crypto.DeriveSubkey(raw, containerSalt, "YTCloud-FolderContainer-v1")
}

// EnsureFolderKeysLocked guarantees every folder (including root) has an automatic 256-bit FolderKeyHex.
// Returns true if any folder key was newly generated.
func EnsureFolderKeysLocked(d *VaultData) bool {
	if d == nil {
		return false
	}
	if d.Folders == nil {
		d.Folders = map[string]*Folder{}
	}
	changed := false
	if _, ok := d.Folders["root"]; !ok {
		d.Folders["root"] = &Folder{
			ID:        "root",
			ParentID:  "",
			Name:      "Meu YTCloud",
			CreatedAt: time.Now().Format(time.RFC3339),
		}
		changed = true
	}
	for _, f := range d.Folders {
		if f.FolderKeyHex == "" {
			if k, err := crypto.RandomHex(32); err == nil {
				f.FolderKeyHex = k
				changed = true
			}
		}
	}
	return changed
}

// EnsureAndGetFolderKey returns the 256-bit key hex for folderID (falling back to root if needed),
// generating and persisting it automatically if missing.
func (s *Store) EnsureAndGetFolderKey(folderID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.unlocked {
		return "", errors.New("cofre bloqueado")
	}
	if folderID == "" {
		folderID = "root"
	}
	f, ok := s.data.Folders[folderID]
	if !ok {
		f = s.data.Folders["root"]
	}
	if f == nil {
		EnsureFolderKeysLocked(s.data)
		f = s.data.Folders["root"]
	}
	if f.FolderKeyHex == "" {
		k, err := crypto.RandomHex(32)
		if err != nil {
			return "", err
		}
		f.FolderKeyHex = k
		_ = s.saveLocked()
	}
	return f.FolderKeyHex, nil
}

// ResolveContainerKey returns the exact AES-256 key for a known container in this vault,
// supporting both per-folder keys (KeyScope == "folder") and legacy master-DEK containers.
func (s *Store) ResolveContainerKey(c *VideoContainer, salt []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.unlocked {
		return nil, errors.New("cofre bloqueado")
	}
	if len(salt) == 0 && c != nil {
		salt = c.ContainerSalt
	}
	if c != nil && (c.KeyScope == "folder" || c.FolderKeyHex != "") {
		fk := c.FolderKeyHex
		if fk == "" && c.FolderID != "" {
			if f, ok := s.data.Folders[c.FolderID]; ok {
				fk = f.FolderKeyHex
			}
		}
		if fk != "" {
			return DeriveFolderContainerKey(fk, salt)
		}
	}
	return crypto.DeriveSubkey(s.dek, salt, "YTCloud-VideoContainer-v1")
}

// TryDecryptVaultManifest transparently decrypts ANY container manifest created by this vault
// using the master vault alone — checking the container's recorded folder key, legacy vault DEK,
// and all folder keys stored in the vault!
func (s *Store) TryDecryptVaultManifest(salt []byte, contUUID string, encManifest []byte) ([]byte, []byte, error) {
	s.mu.RLock()
	if !s.unlocked {
		s.mu.RUnlock()
		return nil, nil, errors.New("cofre bloqueado")
	}
	aad := []byte("YTCloud-Manifest:" + contUUID)

	// Build prioritized list of candidate folder keys + legacy DEK
	var folderKeys []string
	seenFK := map[string]bool{}
	addFK := func(fk string) {
		if fk != "" && !seenFK[fk] {
			seenFK[fk] = true
			folderKeys = append(folderKeys, fk)
		}
	}
	if c, ok := s.data.Containers[contUUID]; ok {
		addFK(c.FolderKeyHex)
		if c.FolderID != "" {
			if f, fok := s.data.Folders[c.FolderID]; fok {
				addFK(f.FolderKeyHex)
			}
		}
	}
	for _, f := range s.data.Folders {
		addFK(f.FolderKeyHex)
	}
	for _, c := range s.data.Containers {
		addFK(c.FolderKeyHex)
	}
	dekCopy := append([]byte(nil), s.dek...)
	s.mu.RUnlock()
	defer crypto.ZeroBytes(dekCopy)

	// 1. Try known container's folder key first (fast path for new folder-encrypted videos)
	if len(folderKeys) > 0 {
		for i, fk := range folderKeys {
			if i > 0 {
				break
			}
			if key, err := DeriveFolderContainerKey(fk, salt); err == nil {
				if plain, decErr := crypto.DecryptAuto(key, encManifest, aad); decErr == nil {
					return key, plain, nil
				}
				crypto.ZeroBytes(key)
			}
		}
	}

	// 2. Try legacy master DEK (fast path for all existing containers)
	if legacyKey, err := crypto.DeriveSubkey(dekCopy, salt, "YTCloud-VideoContainer-v1"); err == nil {
		if plain, decErr := crypto.DecryptAuto(legacyKey, encManifest, aad); decErr == nil {
			return legacyKey, plain, nil
		}
		crypto.ZeroBytes(legacyKey)
	}

	// 3. Fallback: try remaining folder keys stored in the vault (so even deleted/unlinked videos open seamlessly!)
	for i := 1; i < len(folderKeys); i++ {
		if key, err := DeriveFolderContainerKey(folderKeys[i], salt); err == nil {
			if plain, decErr := crypto.DecryptAuto(key, encManifest, aad); decErr == nil {
				return key, plain, nil
			}
			crypto.ZeroBytes(key)
		}
	}

	return nil, nil, errors.New("este vídeo não pertence a este cofre ou a chave é diferente")
}

// ExportEncryptedSnapshot creates a standalone password-encrypted snapshot (.ytcat or Video-Index payload).
func (s *Store) ExportEncryptedSnapshot(password string, includeThumbs bool) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.unlocked {
		return nil, errors.New("cofre bloqueado")
	}

	// Clone data to optionally strip thumbnails for lean catalog sync
	cloneBytes, err := json.Marshal(s.data)
	if err != nil {
		return nil, err
	}
	var snap VaultData
	if err := json.Unmarshal(cloneBytes, &snap); err != nil {
		return nil, err
	}
	if !includeThumbs {
		for _, f := range snap.Files {
			f.PreviewBase64 = ""
		}
	}
	// Never embed OAuth tokens inside remote snapshot unless needed; keep client ID/secret optional
	snap.YouTubeAuth = nil

	payload := struct {
		Header *VaultHeader `json:"header"`
		Data   *VaultData   `json:"data"`
	}{
		Header: s.header,
		Data:   &snap,
	}
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	comp := enc.EncodeAll(jsonBytes, nil)

	return crypto.EncryptWithPassword(password, comp, []byte("YTCloud-CatalogSnapshot-v1"))
}

// RestoreFromEncryptedSnapshot restores a complete vault (including original DEK so all existing YouTube videos work!)
// on any new PC using only the master password.
func (s *Store) RestoreFromEncryptedSnapshot(snapshotBlob []byte, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	comp, err := crypto.DecryptWithPassword(password, snapshotBlob, []byte("YTCloud-CatalogSnapshot-v1"))
	if err != nil {
		return err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return err
	}
	defer dec.Close()
	jsonBytes, err := dec.DecodeAll(comp, nil)
	if err != nil {
		return fmt.Errorf("erro descomprimindo catálogo: %w", err)
	}

	var payload struct {
		Header *VaultHeader `json:"header"`
		Data   *VaultData   `json:"data"`
	}
	if err := json.Unmarshal(jsonBytes, &payload); err != nil || payload.Header == nil || payload.Data == nil {
		return errors.New("snapshot de catálogo inválido")
	}

	dek, err := crypto.UnlockKeyBundle(payload.Header.KeyBundle, password)
	if err != nil {
		return err
	}

	s.header = payload.Header
	s.data = payload.Data
	if s.data.Folders == nil {
		s.data.Folders = map[string]*Folder{}
	}
	if _, ok := s.data.Folders["root"]; !ok {
		s.data.Folders["root"] = &Folder{ID: "root", Name: "Meu YTCloud", CreatedAt: time.Now().Format(time.RFC3339)}
	}
	if s.data.Collections == nil {
		s.data.Collections = map[string]*Collection{}
	}
	if s.data.Files == nil {
		s.data.Files = map[string]*FileEntry{}
	}
	if s.data.Containers == nil {
		s.data.Containers = map[string]*VideoContainer{}
	}
	EnsureFolderKeysLocked(s.data)
	s.dek = dek
	s.unlocked = true
	return s.saveLocked()
}

// Read executes a read-only callback under the store read lock.
func (s *Store) Read(fn func(d *VaultData) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.unlocked {
		return errors.New("cofre bloqueado")
	}
	return fn(s.data)
}

// Update executes a mutating callback, bumps DataRevision (marking Profile Backup as dirty), and persists atomically.
func (s *Store) Update(fn func(d *VaultData) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.unlocked {
		return errors.New("cofre bloqueado")
	}
	if err := fn(s.data); err != nil {
		return err
	}
	s.data.DataRevision++
	return s.saveLocked()
}

// UpdateSystem persists internal state (like PendingDownloads or SyncedRevision) without marking Profile Backup dirty.
func (s *Store) UpdateSystem(fn func(d *VaultData) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.unlocked {
		return errors.New("cofre bloqueado")
	}
	if err := fn(s.data); err != nil {
		return err
	}
	return s.saveLocked()
}

// EnsureFolderPath creates nested virtual folders under parentID (e.g. "Viagem 1998/Praia") and returns the leaf folder ID.
func (s *Store) EnsureFolderPathLocked(d *VaultData, parentID, relDir string) string {
	if parentID == "" {
		parentID = "root"
	}
	relDir = strings.Trim(filepath.ToSlash(relDir), "/")
	if relDir == "" || relDir == "." {
		return parentID
	}
	curr := parentID
	for _, seg := range strings.Split(relDir, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." {
			continue
		}
		found := ""
		for _, f := range d.Folders {
			if f.ParentID == curr && strings.EqualFold(f.Name, seg) {
				found = f.ID
				break
			}
		}
		if found != "" {
			curr = found
			continue
		}
		id, _ := crypto.RandomHex(6)
		fk, _ := crypto.RandomHex(32)
		d.Folders[id] = &Folder{
			ID:           id,
			ParentID:     curr,
			Name:         seg,
			FolderKeyHex: fk,
			CreatedAt:    time.Now().Format(time.RFC3339),
		}
		curr = id
	}
	return curr
}

// SearchFiles executes multi-criteria search (name, folder, collection, tag, date range, media type, container).
func (s *Store) SearchFiles(q SearchQuery) ([]*FileEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.unlocked {
		return nil, errors.New("cofre bloqueado")
	}

	var allowedFolders map[string]bool
	if q.FolderID != "" {
		allowedFolders = map[string]bool{q.FolderID: true}
		if q.Recursive {
			changed := true
			for changed {
				changed = false
				for _, f := range s.data.Folders {
					if allowedFolders[f.ParentID] && !allowedFolders[f.ID] {
						allowedFolders[f.ID] = true
						changed = true
					}
				}
			}
		}
	}

	needle := strings.ToLower(strings.TrimSpace(q.Query))
	tagNeedle := strings.ToLower(strings.TrimSpace(q.Tag))
	var results []*FileEntry

	for _, f := range s.data.Files {
		if allowedFolders != nil && !allowedFolders[f.FolderID] {
			continue
		}
		if q.ContainerID != "" && f.ContainerID != q.ContainerID {
			continue
		}
		if q.CollectionID != "" {
			has := false
			for _, cid := range f.CollectionIDs {
				if cid == q.CollectionID {
					has = true
					break
				}
			}
			if !has {
				continue
			}
		}
		if tagNeedle != "" {
			has := false
			for _, t := range f.Tags {
				if strings.ToLower(t) == tagNeedle {
					has = true
					break
				}
			}
			if !has {
				continue
			}
		}
		if q.OnlyNoDate && strings.TrimSpace(f.EffectiveDate) != "" {
			continue
		}
		if q.DateFrom != "" {
			if f.EffectiveDate == "" || f.EffectiveDate < q.DateFrom {
				continue
			}
		}
		if q.DateTo != "" {
			// Compare prefix safely so "1999" matches "1999-12-31"
			if f.EffectiveDate == "" || (len(f.EffectiveDate) >= len(q.DateTo) && f.EffectiveDate[:len(q.DateTo)] > q.DateTo) {
				continue
			}
		}
		if q.MediaType != "" {
			if !matchesMediaType(f, q.MediaType) {
				continue
			}
		}
		if needle != "" {
			hay := strings.ToLower(f.DisplayName + " " + f.OriginalName + " " + f.LocationLabel + " " + f.Notes + " " + strings.Join(f.Tags, " "))
			if !strings.Contains(hay, needle) {
				continue
			}
		}
		results = append(results, f)
	}

	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		switch q.SortBy {
		case "date_asc":
			return effectiveSortKey(a) < effectiveSortKey(b)
		case "name_asc":
			return strings.ToLower(a.DisplayName) < strings.ToLower(b.DisplayName)
		case "size_desc":
			return a.SizeBytes > b.SizeBytes
		case "added_desc":
			return a.AddedAt > b.AddedAt
		default: // "date_desc"
			return effectiveSortKey(a) > effectiveSortKey(b)
		}
	})

	return results, nil
}

func effectiveSortKey(f *FileEntry) string {
	if f.EffectiveDate != "" {
		return f.EffectiveDate + "|" + strings.ToLower(f.DisplayName)
	}
	if f.OriginalDate != "" {
		return f.OriginalDate + "|" + strings.ToLower(f.DisplayName)
	}
	return "0000-00-00|" + strings.ToLower(f.DisplayName)
}

func matchesMediaType(f *FileEntry, mediaType string) bool {
	mt := strings.ToLower(f.MimeType)
	ext := strings.ToLower(filepath.Ext(f.OriginalName))
	switch mediaType {
	case "image":
		return strings.HasPrefix(mt, "image/") || ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" || ext == ".gif"
	case "video":
		return strings.HasPrefix(mt, "video/") || ext == ".mp4" || ext == ".mkv" || ext == ".mov" || ext == ".avi"
	case "audio":
		return strings.HasPrefix(mt, "audio/") || ext == ".mp3" || ext == ".wav" || ext == ".flac" || ext == ".m4a"
	case "document":
		return strings.HasPrefix(mt, "text/") || ext == ".pdf" || ext == ".doc" || ext == ".docx" || ext == ".xls" || ext == ".xlsx" || ext == ".txt" || ext == ".md"
	default:
		return true
	}
}

// ApplyBatchMetadata updates metadata for multiple files at once (ideal for old photo batches!).
func (s *Store) ApplyBatchMetadata(upd BatchMetadataUpdate) (int, error) {
	updated := 0
	err := s.Update(func(d *VaultData) error {
		for _, id := range upd.FileIDs {
			f, ok := d.Files[id]
			if !ok {
				continue
			}
			if upd.EffectiveDate != nil {
				f.EffectiveDate = strings.TrimSpace(*upd.EffectiveDate)
			}
			if upd.DatePrecision != nil {
				f.DatePrecision = strings.TrimSpace(*upd.DatePrecision)
			}
			if upd.ShiftDays != 0 {
				if t, ok := packer.ParseFlexibleDate(f.EffectiveDate); ok {
					f.EffectiveDate = t.AddDate(0, 0, upd.ShiftDays).Format("2006-01-02 15:04:05")
				}
			}
			if upd.LocationLabel != nil {
				f.LocationLabel = strings.TrimSpace(*upd.LocationLabel)
			}
			if upd.MoveToFolderID != nil {
				if _, exists := d.Folders[*upd.MoveToFolderID]; exists {
					f.FolderID = *upd.MoveToFolderID
				}
			}
			if upd.AddCollection != "" {
				if _, exists := d.Collections[upd.AddCollection]; exists {
					has := false
					for _, c := range f.CollectionIDs {
						if c == upd.AddCollection {
							has = true
							break
						}
					}
					if !has {
						f.CollectionIDs = append(f.CollectionIDs, upd.AddCollection)
					}
				}
			}
			for _, nt := range upd.AddTags {
				nt = strings.TrimSpace(nt)
				if nt == "" {
					continue
				}
				has := false
				for _, et := range f.Tags {
					if strings.EqualFold(et, nt) {
						has = true
						break
					}
				}
				if !has {
					f.Tags = append(f.Tags, nt)
				}
			}
			updated++
		}
		return nil
	})
	return updated, err
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	hdrBytes, err := json.Marshal(s.header)
	if err != nil {
		return err
	}
	bodyJSON, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return err
	}
	comp := enc.EncodeAll(bodyJSON, nil)
	enc.Close()

	encBody, err := crypto.EncryptPQ(s.dek, comp, []byte("YTCloud-VaultBody:"+s.header.VaultID))
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	buf.WriteString(VaultFileMagic)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(hdrBytes)))
	buf.Write(hdrBytes)
	buf.Write(encBody)

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func parseVaultFile(raw []byte) (*VaultHeader, []byte, error) {
	if len(raw) < 8 || string(raw[:4]) != VaultFileMagic {
		return nil, nil, errors.New("arquivo de cofre inválido")
	}
	hLen := int(binary.LittleEndian.Uint32(raw[4:8]))
	if hLen <= 0 || 8+hLen >= len(raw) {
		return nil, nil, errors.New("cabeçalho do cofre corrompido")
	}
	var hdr VaultHeader
	if err := json.Unmarshal(raw[8:8+hLen], &hdr); err != nil {
		return nil, nil, err
	}
	if hdr.Magic != HeaderMagicStr {
		return nil, nil, errors.New("versão de cofre não suportada")
	}
	return &hdr, raw[8+hLen:], nil
}

func decryptVaultBody(dek []byte, vaultID string, encBody []byte) (*VaultData, error) {
	comp, err := crypto.DecryptAuto(dek, encBody, []byte("YTCloud-VaultBody:"+vaultID))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	bodyJSON, err := dec.DecodeAll(comp, nil)
	if err != nil {
		return nil, err
	}
	var d VaultData
	if err := json.Unmarshal(bodyJSON, &d); err != nil {
		return nil, err
	}
	if d.Folders == nil {
		d.Folders = map[string]*Folder{}
	}
	if d.Collections == nil {
		d.Collections = map[string]*Collection{}
	}
	if d.Files == nil {
		d.Files = map[string]*FileEntry{}
	}
	if d.Containers == nil {
		d.Containers = map[string]*VideoContainer{}
	}
	if d.SyncGroups == nil {
		d.SyncGroups = map[string]*SyncGroup{}
	}
	if d.PendingDownloads == nil {
		d.PendingDownloads = map[string]*PendingDownload{}
	}
	// One-time v2 settings migration for legacy vaults: 4K max + Unlisted default
	if d.SettingsVersion < 2 {
		if d.Settings.DefaultResolution == "" || d.Settings.DefaultResolution == "8k" {
			d.Settings.DefaultResolution = "4k"
		}
		if d.Settings.DefaultProfile == "" || d.Settings.DefaultProfile == "ycbcr-dual" {
			d.Settings.DefaultProfile = "y8-gray"
		}
		if d.Settings.DefaultPrivacy == "" || d.Settings.DefaultPrivacy == "private" {
			d.Settings.DefaultPrivacy = "unlisted"
		}
		d.SettingsVersion = 2
	}
	// One-time v3 settings migration: upgrade default profile to y8-gray (8 Tons de Cinza DCT 8x8 + preset medium)
	if d.SettingsVersion < 3 {
		if d.Settings.DefaultProfile == "" || d.Settings.DefaultProfile == "y4-gray" || d.Settings.DefaultProfile == "ycbcr-dual" {
			d.Settings.DefaultProfile = "y8-gray"
		}
		d.SettingsVersion = 3
	}
	return &d, nil
}

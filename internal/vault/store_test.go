package vault

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVaultCreateUnlockPasswordChangeAndSnapshotRestore(t *testing.T) {
	dir := t.TempDir()
	vp := filepath.Join(dir, "vault.ytdb")

	s := NewStore(vp)
	if err := s.Create("Cofre Teste", "SenhaSuperForte!2026"); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Add folder, collection, and file with approximate old photo date
	err := s.Update(func(d *VaultData) error {
		fid := s.EnsureFolderPathLocked(d, "root", "Fotos Antigas/1998")
		d.Collections["col1"] = &Collection{ID: "col1", Name: "Infância", Color: "#38bdf8"}
		d.Files["f1"] = &FileEntry{
			ID:            "f1",
			FolderID:      fid,
			OriginalName:  "IMG_001.jpg",
			DisplayName:   "Praia 1998",
			EffectiveDate: "1998-07-01",
			DatePrecision: "month",
			LocationLabel: "Praia Grande",
			CollectionIDs: []string{"col1"},
			Tags:          []string{"familia", "praia"},
			SizeBytes:     204800,
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	// Derive a container key BEFORE password change
	salt := []byte("0123456789abcdef")
	keyBefore, err := s.DeriveContainerKey(salt)
	if err != nil {
		t.Fatalf("DeriveContainerKey failed: %v", err)
	}

	// Change master password — DEK must stay identical!
	if err := s.ChangePassword("SenhaSuperForte!2026", "NovaSenhaForte#99"); err != nil {
		t.Fatalf("ChangePassword failed: %v", err)
	}
	s.Lock()

	// Old password must fail
	if err := s.Unlock("SenhaSuperForte!2026"); err == nil {
		t.Fatalf("Expected old password to fail")
	}
	// New password must succeed
	if err := s.Unlock("NovaSenhaForte#99"); err != nil {
		t.Fatalf("Unlock with new password failed: %v", err)
	}
	keyAfter, _ := s.DeriveContainerKey(salt)
	if string(keyBefore) != string(keyAfter) {
		t.Fatalf("Container key changed after password rotation — existing videos would break!")
	}

	// Search by collection, tag, and date range
	res, err := s.SearchFiles(SearchQuery{CollectionID: "col1", DateFrom: "1995", DateTo: "2000", Tag: "praia"})
	if err != nil || len(res) != 1 || res[0].DisplayName != "Praia 1998" {
		t.Fatalf("SearchFiles failed: len=%d err=%v", len(res), err)
	}

	// Export encrypted snapshot & restore onto a brand new PC (new vault path)
	snap, err := s.ExportEncryptedSnapshot("NovaSenhaForte#99", true)
	if err != nil {
		t.Fatalf("ExportEncryptedSnapshot failed: %v", err)
	}
	vp2 := filepath.Join(dir, "new_pc", "vault.ytdb")
	s2 := NewStore(vp2)
	if err := s2.RestoreFromEncryptedSnapshot(snap, "NovaSenhaForte#99"); err != nil {
		t.Fatalf("RestoreFromEncryptedSnapshot failed: %v", err)
	}
	keyRestored, _ := s2.DeriveContainerKey(salt)
	if string(keyRestored) != string(keyBefore) {
		t.Fatalf("Restored vault DEK mismatch!")
	}
	if _, err := os.Stat(vp2); err != nil {
		t.Fatalf("Restored vault file missing: %v", err)
	}
}

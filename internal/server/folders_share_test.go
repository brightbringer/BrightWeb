package server

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ytcloud/internal/crypto"
	"ytcloud/internal/vault"
)

func newTestServerWithVault(t *testing.T) (*Server, *vault.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store := vault.NewStore(filepath.Join(dir, "vault.ytdb"))
	if err := store.Create("Cofre Teste", "SenhaForte#2026"); err != nil {
		t.Fatalf("create vault: %v", err)
	}
	srv, err := New(store)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, store, dir
}

func doAuthedJSON(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&rdr).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &rdr)
	req.Header.Set("X-YTCloud-Token", srv.Token())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.authMiddleware(srv.mux).ServeHTTP(rec, req)
	return rec
}

func TestPerFolderKeysMasterTransparencyAndShareCode(t *testing.T) {
	srv, store, _ := newTestServerWithVault(t)

	// 1. Create two independent folders via API
	recA := doAuthedJSON(t, srv, "POST", "/api/folders", map[string]any{"parent_id": "root", "name": "Fotos 1998"})
	if recA.Code != 200 {
		t.Fatalf("create folder A failed (%d): %s", recA.Code, recA.Body.String())
	}
	var folA vault.Folder
	_ = json.Unmarshal(recA.Body.Bytes(), &folA)
	if folA.FolderKeyHex != "" {
		t.Fatalf("folder key should be hidden in create response")
	}

	recB := doAuthedJSON(t, srv, "POST", "/api/folders", map[string]any{"parent_id": "root", "name": "Documentos Privados"})
	var folB vault.Folder
	_ = json.Unmarshal(recB.Body.Bytes(), &folB)

	// 2. Fetch props of both folders -> must have distinct 256-bit (64-hex) automatic keys
	recPropsA := doAuthedJSON(t, srv, "GET", "/api/folders/"+folA.ID+"/props", nil)
	if recPropsA.Code != 200 {
		t.Fatalf("props A failed (%d): %s", recPropsA.Code, recPropsA.Body.String())
	}
	var propsA, propsB map[string]any
	_ = json.Unmarshal(recPropsA.Body.Bytes(), &propsA)
	recPropsB := doAuthedJSON(t, srv, "GET", "/api/folders/"+folB.ID+"/props", nil)
	_ = json.Unmarshal(recPropsB.Body.Bytes(), &propsB)

	keyA, _ := propsA["folder_key_hex"].(string)
	keyB, _ := propsB["folder_key_hex"].(string)
	if len(keyA) != 64 || len(keyB) != 64 || keyA == keyB {
		t.Fatalf("expected distinct 64-hex folder keys, got A=%q B=%q", keyA, keyB)
	}

	// 3. Encrypt a container under Folder A's key AND a legacy container under Master DEK
	saltA, _ := crypto.RandomBytes(16)
	contUUIDA := "aaaaaaaaaaaaaaaa"
	contKeyA, err := vault.DeriveFolderContainerKey(keyA, saltA)
	if err != nil {
		t.Fatalf("derive folder key: %v", err)
	}
	plainManifestA := []byte(`{"container_id":"aaaaaaaaaaaaaaaa","files":[]}`)
	encManifestA, err := crypto.EncryptPQ(contKeyA, plainManifestA, []byte("YTCloud-Manifest:"+contUUIDA))
	if err != nil {
		t.Fatalf("encrypt folder manifest: %v", err)
	}

	saltLegacy, _ := crypto.RandomBytes(16)
	contUUIDLeg := "bbbbbbbbbbbbbbbb"
	contKeyLeg, err := store.DeriveContainerKey(saltLegacy)
	if err != nil {
		t.Fatalf("derive legacy key: %v", err)
	}
	plainManifestLeg := []byte(`{"container_id":"bbbbbbbbbbbbbbbb","files":[]}`)
	encManifestLeg, err := crypto.EncryptPQ(contKeyLeg, plainManifestLeg, []byte("YTCloud-Manifest:"+contUUIDLeg))
	if err != nil {
		t.Fatalf("encrypt legacy manifest: %v", err)
	}

	// Register containers & files into Vault
	err = store.Update(func(d *vault.VaultData) error {
		d.Containers[contUUIDA] = &vault.VideoContainer{
			ID:             contUUIDA,
			ShortCode:      "YTC-FOLDER-A",
			YouTubeVideoID: "dQw4w9WgXcQ",
			YouTubeURL:     "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			Status:         "verified",
			Privacy:        "public",
			ContainerSalt:  saltA,
			KeyScope:       "folder",
			FolderID:       folA.ID,
			FolderKeyHex:   keyA,
			FileCount:      1,
		}
		d.Files["f1"] = &vault.FileEntry{
			ID:             "f1",
			FolderID:       folA.ID,
			ContainerID:    contUUIDA,
			RelPathInVideo: "praia.jpg",
			OriginalName:   "praia.jpg",
			DisplayName:    "Praia 1998.jpg",
			SizeBytes:      123456,
			EffectiveDate:  "1998-07-14",
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store update: %v", err)
	}

	// 4. Master Vault transparently unlocks BOTH Folder A video and Legacy video without user knowing Folder A key!
	gotKeyA, decA, err := store.TryDecryptVaultManifest(saltA, contUUIDA, encManifestA)
	if err != nil || string(decA) != string(plainManifestA) {
		t.Fatalf("TryDecryptVaultManifest folder failed: err=%v dec=%q", err, string(decA))
	}
	crypto.ZeroBytes(gotKeyA)

	gotKeyLeg, decLeg, err := store.TryDecryptVaultManifest(saltLegacy, contUUIDLeg, encManifestLeg)
	if err != nil || string(decLeg) != string(plainManifestLeg) {
		t.Fatalf("TryDecryptVaultManifest legacy failed: err=%v dec=%q", err, string(decLeg))
	}
	crypto.ZeroBytes(gotKeyLeg)

	// 5. Generate point-in-time Share Code for Folder A
	recShare := doAuthedJSON(t, srv, "POST", "/api/folders/"+folA.ID+"/share-code", map[string]any{"include_subfolders": true})
	if recShare.Code != 200 {
		t.Fatalf("share-code failed (%d): %s", recShare.Code, recShare.Body.String())
	}
	var shareResp struct {
		Code           string `json:"code"`
		FileCount      int    `json:"file_count"`
		ContainerCount int    `json:"container_count"`
	}
	_ = json.Unmarshal(recShare.Body.Bytes(), &shareResp)
	if !strings.HasPrefix(shareResp.Code, vault.FolderSharePrefix) || shareResp.FileCount != 1 || shareResp.ContainerCount != 1 {
		t.Fatalf("unexpected share response: %+v", shareResp)
	}

	// 6. Lock the vault and verify inspect works WITHOUT any vault unlocked!
	store.Lock()
	recInsp := doAuthedJSON(t, srv, "POST", "/api/public/folder-inspect", map[string]any{"code": shareResp.Code})
	if recInsp.Code != 200 {
		t.Fatalf("public folder-inspect while locked failed (%d): %s", recInsp.Code, recInsp.Body.String())
	}
	var inspResp map[string]any
	_ = json.Unmarshal(recInsp.Body.Bytes(), &inspResp)
	if inspResp["folder_name"] != "Fotos 1998" {
		t.Fatalf("expected folder_name Fotos 1998, got %v", inspResp["folder_name"])
	}

	// 7. Verify embedded container key decrypts Folder A's manifest, and CANNOT decrypt Folder B/Legacy!
	parsed, err := vault.DecodeFolderShareCode(shareResp.Code)
	if err != nil || len(parsed.Containers) != 1 {
		t.Fatalf("decode share code: %v", err)
	}
	embKey, err := hex.DecodeString(parsed.Containers[0].KeyHex)
	if err != nil || len(embKey) != 32 {
		t.Fatalf("invalid embedded key")
	}
	decByShare, err := crypto.DecryptAuto(embKey, encManifestA, []byte("YTCloud-Manifest:"+contUUIDA))
	if err != nil || string(decByShare) != string(plainManifestA) {
		t.Fatalf("share key failed to decrypt Folder A manifest: %v", err)
	}
	if _, err := crypto.DecryptAuto(embKey, encManifestLeg, []byte("YTCloud-Manifest:"+contUUIDLeg)); err == nil {
		t.Fatalf("share key must NOT decrypt unrelated container!")
	}
}

func TestContainerDisassociateDeleteAndPublicPrivacy(t *testing.T) {
	srv, store, _ := newTestServerWithVault(t)

	// Verify "public" privacy is saved cleanly in /api/settings
	recSet := doAuthedJSON(t, srv, "POST", "/api/settings", map[string]any{"default_privacy": "public"})
	if recSet.Code != http.StatusOK {
		t.Fatalf("settings public failed (%d): %s", recSet.Code, recSet.Body.String())
	}
	_ = store.Read(func(d *vault.VaultData) error {
		if d.Settings.DefaultPrivacy != "public" {
			t.Fatalf("expected default_privacy=public, got %q", d.Settings.DefaultPrivacy)
		}
		return nil
	})

	// Seed a SyncGroup chain (Base + Delta #1) with files
	_ = store.Update(func(d *vault.VaultData) error {
		d.SyncGroups["SYNC-12345678"] = &vault.SyncGroup{
			ID: "SYNC-12345678", Name: "Projetos", FolderID: "root",
			BaseContainerID: "cbase", LastContainerID: "cdelta1", DeltaCount: 1, MaxDeltas: 10,
		}
		d.Containers["cbase"] = &vault.VideoContainer{
			ID: "cbase", ShortCode: "YTC-BASE", Status: "uploaded", Privacy: "public",
			SyncID: "SYNC-12345678", DeltaIndex: 0, FileCount: 1,
		}
		d.Containers["cdelta1"] = &vault.VideoContainer{
			ID: "cdelta1", ShortCode: "YTC-U1", Status: "staged", Privacy: "unlisted",
			SyncID: "SYNC-12345678", DeltaIndex: 1, FileCount: 1,
		}
		d.Files["f_base"] = &vault.FileEntry{ID: "f_base", FolderID: "root", ContainerID: "cbase", DisplayName: "a.txt"}
		d.Files["f_delta"] = &vault.FileEntry{ID: "f_delta", FolderID: "root", ContainerID: "cdelta1", DisplayName: "b.txt"}
		return nil
	})

	// 1. Disassociate files from cbase (keep container)
	recDis := doAuthedJSON(t, srv, "POST", "/api/containers/cbase/disassociate", map[string]any{"remove_container": false})
	if recDis.Code != 200 {
		t.Fatalf("disassociate failed (%d): %s", recDis.Code, recDis.Body.String())
	}
	_ = store.Read(func(d *vault.VaultData) error {
		if d.Files["f_base"].ContainerID != "" {
			t.Fatalf("expected f_base disassociated, got %q", d.Files["f_base"].ContainerID)
		}
		if d.Containers["cbase"].FileCount != 0 {
			t.Fatalf("expected cbase file_count=0, got %d", d.Containers["cbase"].FileCount)
		}
		return nil
	})

	// 2. Delete cdelta1 with file_mode=delete -> removes cdelta1 & f_delta and updates SyncGroup DeltaCount to 0!
	recDel := doAuthedJSON(t, srv, "POST", "/api/containers/cdelta1/delete", map[string]any{"file_mode": "delete"})
	if recDel.Code != 200 {
		t.Fatalf("delete container failed (%d): %s", recDel.Code, recDel.Body.String())
	}
	_ = store.Read(func(d *vault.VaultData) error {
		if _, exists := d.Containers["cdelta1"]; exists {
			t.Fatalf("cdelta1 should be deleted")
		}
		if _, exists := d.Files["f_delta"]; exists {
			t.Fatalf("f_delta should be deleted")
		}
		if sg := d.SyncGroups["SYNC-12345678"]; sg == nil || sg.DeltaCount != 0 || sg.LastContainerID != "cbase" {
			t.Fatalf("unexpected sync group after delta removal: %+v", sg)
		}
		return nil
	})
}

func TestFolderReorderSiblingOnlyAndWrapParent(t *testing.T) {
	srv, store, _ := newTestServerWithVault(t)

	mkFolder := func(parent, name string) string {
		rec := doAuthedJSON(t, srv, "POST", "/api/folders", map[string]any{"parent_id": parent, "name": name})
		if rec.Code != 200 {
			t.Fatalf("create folder %s failed: %s", name, rec.Body.String())
		}
		var f vault.Folder
		_ = json.Unmarshal(rec.Body.Bytes(), &f)
		return f.ID
	}

	f1 := mkFolder("root", "Alpha")
	f2 := mkFolder("root", "Beta")
	sub := mkFolder(f1, "Filha")

	// 1. Reorder siblings f2 before f1 under root -> must succeed
	recRe := doAuthedJSON(t, srv, "POST", "/api/folders/reorder", map[string]any{
		"parent_id":   "root",
		"ordered_ids": []string{f2, f1},
	})
	if recRe.Code != 200 {
		t.Fatalf("reorder siblings failed (%d): %s", recRe.Code, recRe.Body.String())
	}
	_ = store.Read(func(d *vault.VaultData) error {
		if d.Folders[f2].SortOrder != 1 || d.Folders[f1].SortOrder != 2 {
			t.Fatalf("unexpected sort orders: f2=%d f1=%d", d.Folders[f2].SortOrder, d.Folders[f1].SortOrder)
		}
		return nil
	})

	// 2. Attempt to reorder child 'sub' at root level -> MUST BE REJECTED (cannot pull folder out of parent!)
	recBad := doAuthedJSON(t, srv, "POST", "/api/folders/reorder", map[string]any{
		"parent_id":   "root",
		"ordered_ids": []string{sub, f2, f1},
	})
	if recBad.Code == 200 {
		t.Fatalf("expected reorder across parents to fail, got 200")
	}

	// 3. Wrap 'sub' inside a new "Pasta Mãe" -> creates new parent under f1 and moves 'sub' into it!
	recWrap := doAuthedJSON(t, srv, "POST", "/api/folders/"+sub+"/wrap-parent", map[string]any{"name": "Pasta Mae"})
	if recWrap.Code != 200 {
		t.Fatalf("wrap-parent failed (%d): %s", recWrap.Code, recWrap.Body.String())
	}
	var wrapResp struct {
		ParentFolder vault.Folder `json:"parent_folder"`
	}
	_ = json.Unmarshal(recWrap.Body.Bytes(), &wrapResp)
	_ = store.Read(func(d *vault.VaultData) error {
		if d.Folders[wrapResp.ParentFolder.ID].ParentID != f1 {
			t.Fatalf("expected new parent under f1, got %q", d.Folders[wrapResp.ParentFolder.ID].ParentID)
		}
		if d.Folders[sub].ParentID != wrapResp.ParentFolder.ID {
			t.Fatalf("expected sub nested inside new parent %q, got %q", wrapResp.ParentFolder.ID, d.Folders[sub].ParentID)
		}
		return nil
	})
}

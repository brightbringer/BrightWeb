package browser

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDNSBookMinhaListaHierarchyImportExportAndResolve(t *testing.T) {
	srv, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	// Isolate DNS file path in t.TempDir() so test never touches user's real %LOCALAPPDATA% list
	srv.mu.Lock()
	srv.dnsFilePath = filepath.Join(t.TempDir(), "brightweb_dns.json")
	srv.dnsBook = defaultDNSBook()
	srv.mu.Unlock()

	h := srv.Handler()
	doAPI := func(method, url string, body any) *httptest.ResponseRecorder {
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, url, rdr)
		req.Header.Set("X-YTBrowser-Token", srv.Token())
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 1. Default "Minha Lista" contains protected "Meus Sites" folder
	rec := doAPI(http.MethodGet, "/api/dns", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /api/dns -> %d", rec.Code)
	}
	var book DNSBook
	if err := json.Unmarshal(rec.Body.Bytes(), &book); err != nil {
		t.Fatal(err)
	}
	if book.Name != "Minha Lista" {
		t.Fatalf("expected book name 'Minha Lista', got %q", book.Name)
	}
	hasMySites := false
	for _, e := range book.Entries {
		if e.ID == DefaultMySitesFolderID && e.Name == "Meus Sites" && e.Protected {
			hasMySites = true
		}
	}
	if !hasMySites {
		t.Fatalf("missing default protected 'Meus Sites' folder")
	}

	// 2. Save a parent site inside "Meus Sites" WITH password + alias
	recParent := doAPI(http.MethodPost, "/api/dns/save", DNSEntry{
		Kind:      "site",
		Name:      "Portal Jogos",
		Alias:     "jogos.btw",
		Source:    "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Passwords: []string{"segredo-pq-1"},
		ParentID:  DefaultMySitesFolderID,
		Favorite:  true,
	})
	if recParent.Code != 200 {
		t.Fatalf("save parent site -> %d: %s", recParent.Code, recParent.Body.String())
	}
	var saveResp struct {
		Entry DNSEntry `json:"entry"`
	}
	_ = json.Unmarshal(recParent.Body.Bytes(), &saveResp)
	parentSiteID := saveResp.Entry.ID

	// 3. Save a child site DIRECTLY inside the parent site ("site dentro de site"!)
	recChild := doAPI(http.MethodPost, "/api/dns/save", DNSEntry{
		Kind:     "site",
		Name:     "DOOM 1993",
		Alias:    "doom.btw",
		Source:   "https://www.youtube.com/watch?v=aaaaaaaaaaa",
		ParentID: parentSiteID,
	})
	if recChild.Code != 200 {
		t.Fatalf("save child site inside site -> %d: %s", recChild.Code, recChild.Body.String())
	}
	var childResp struct {
		Entry DNSEntry `json:"entry"`
	}
	_ = json.Unmarshal(recChild.Body.Bytes(), &childResp)
	if childResp.Entry.ParentID != parentSiteID {
		t.Fatalf("expected child site nested under %s, got %s", parentSiteID, childResp.Entry.ParentID)
	}

	// 4. DNS Resolution: btweb://jogos.btw/fase2.html resolves URL + saved password automatically!
	resolved, pws, disp, hit := srv.resolveDNSAddress("btweb://jogos.btw/fase2.html")
	if !hit || resolved != "btweb://dQw4w9WgXcQ/fase2.html" || len(pws) != 1 || pws[0] != "segredo-pq-1" || disp != "btweb://jogos.btw/fase2.html" {
		t.Fatalf("unexpected DNS resolution: hit=%v res=%q pws=%v disp=%q", hit, resolved, pws, disp)
	}

	// 5. Prevent cycle when moving parent inside its own child site!
	recCycle := doAPI(http.MethodPost, "/api/dns/reorder", map[string]any{
		"id":            parentSiteID,
		"new_parent_id": childResp.Entry.ID,
	})
	if recCycle.Code == 200 {
		t.Fatalf("expected cycle prevention error (400), got 200")
	}

	// 6. Export without passwords strips secrets cleanly, export with passwords keeps them!
	recExpSafe := doAPI(http.MethodGet, "/api/dns/export?include_passwords=0", nil)
	if strings.Contains(recExpSafe.Body.String(), "segredo-pq-1") {
		t.Fatalf("safe export leaked password!")
	}
	recExpFull := doAPI(http.MethodGet, "/api/dns/export?include_passwords=1", nil)
	if !strings.Contains(recExpFull.Body.String(), "segredo-pq-1") {
		t.Fatalf("full export missing password!")
	}

	// 7. Import CSV (like Google Sheets export)
	csvData := "nome,alias,link,senha\nMeu Blog,blog.btw,https://youtu.be/bbbbbbbbbbb,pw123\n"
	recImp := doAPI(http.MethodPost, "/api/dns/import", map[string]any{
		"raw_content":      csvData,
		"target_parent_id": DefaultMySitesFolderID,
	})
	if recImp.Code != 200 {
		t.Fatalf("CSV import failed: %d %s", recImp.Code, recImp.Body.String())
	}
	resBlog, pwBlog, _, hitBlog := srv.resolveDNSAddress("blog.btw")
	if !hitBlog || resBlog != "https://youtu.be/bbbbbbbbbbb" || len(pwBlog) != 1 || pwBlog[0] != "pw123" {
		t.Fatalf("imported CSV alias failed to resolve: hit=%v res=%q pw=%v", hitBlog, resBlog, pwBlog)
	}

	// 8. Cannot delete protected "Meus Sites"
	recDelProt := doAPI(http.MethodPost, "/api/dns/delete", map[string]any{"id": DefaultMySitesFolderID})
	if recDelProt.Code == 200 {
		t.Fatalf("should not allow deleting protected 'Meus Sites'")
	}
}

package browser

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"ytcloud/internal/crypto"
	"ytcloud/internal/media"
	"ytcloud/internal/youtube"
)

const DefaultMySitesFolderID = "meus-sites"

// DNSEntry represents either a folder or a saved BrightNet site inside "Minha Lista".
// Both folders AND sites can act as parents (allowing folders, subfolders, AND "site inside site"!).
type DNSEntry struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"` // "folder" | "site"
	Name      string   `json:"name"`
	Alias     string   `json:"alias,omitempty"`     // optional short DNS alias (e.g. "doom.btw")
	Source    string   `json:"source,omitempty"`    // YouTube URL, btweb://ID1+ID2, HTTP URL or local .mp4
	Passwords []string `json:"passwords,omitempty"` // optional saved passwords for YTPUB001 encrypted sites
	ParentID  string   `json:"parent_id,omitempty"` // "" = root, "meus-sites" = Meus Sites, or any folder/site ID
	Order     int      `json:"order"`
	Favorite  bool     `json:"favorite,omitempty"`
	Protected bool     `json:"protected,omitempty"` // true for default "Meus Sites" folder
	Notes     string   `json:"notes,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

// DNSBook holds the user's personal DNS address book ("Minha Lista").
type DNSBook struct {
	Name    string     `json:"name"`
	Version int        `json:"version"`
	Entries []DNSEntry `json:"entries"`
}

func dnsBookPath() string {
	return filepath.Join(media.DefaultAppDir(), "brightweb_dns.json")
}

func defaultDNSBook() DNSBook {
	now := time.Now().Format(time.RFC3339)
	return DNSBook{
		Name:    "Minha Lista",
		Version: 1,
		Entries: []DNSEntry{
			{
				ID:        DefaultMySitesFolderID,
				Kind:      "folder",
				Name:      "Meus Sites",
				ParentID:  "",
				Order:     0,
				Protected: true,
				Notes:     "Seus próprios sites publicados na BrightNet.",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
}

func normalizeDNSBook(b DNSBook) DNSBook {
	if strings.TrimSpace(b.Name) == "" {
		b.Name = "Minha Lista"
	}
	if b.Version <= 0 {
		b.Version = 1
	}
	if b.Entries == nil {
		b.Entries = []DNSEntry{}
	}
	hasMySites := false
	seen := make(map[string]bool, len(b.Entries))
	clean := make([]DNSEntry, 0, len(b.Entries)+1)
	for _, e := range b.Entries {
		if strings.TrimSpace(e.ID) == "" {
			id, _ := crypto.RandomHex(6)
			e.ID = "dns_" + id
		}
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		if e.Kind != "folder" && e.Kind != "site" {
			if strings.TrimSpace(e.Source) != "" {
				e.Kind = "site"
			} else {
				e.Kind = "folder"
			}
		}
		if e.ID == DefaultMySitesFolderID {
			e.Kind = "folder"
			e.Protected = true
			if strings.TrimSpace(e.Name) == "" {
				e.Name = "Meus Sites"
			}
			hasMySites = true
		}
		e.Alias = normalizeDNSAlias(e.Alias)
		clean = append(clean, e)
	}
	if !hasMySites {
		def := defaultDNSBook().Entries[0]
		clean = append([]DNSEntry{def}, clean...)
	}
	// Clear dangling parent IDs cleanly
	for i := range clean {
		if clean[i].ParentID != "" && (!seen[clean[i].ParentID] && clean[i].ParentID != DefaultMySitesFolderID) {
			clean[i].ParentID = ""
		}
		if clean[i].ParentID == clean[i].ID {
			clean[i].ParentID = ""
		}
	}
	b.Entries = clean
	return b
}

func normalizeDNSAlias(raw string) string {
	a := strings.TrimSpace(raw)
	if a == "" {
		return ""
	}
	a, _ = stripOpticalScheme(a)
	a = strings.Trim(a, "/")
	if idx := strings.IndexByte(a, '/'); idx >= 0 {
		a = a[:idx]
	}
	a = strings.ToLower(strings.TrimSpace(a))
	a = strings.ReplaceAll(a, " ", "-")
	return a
}

func (s *Server) effectiveDNSPath() string {
	if s.dnsFilePath != "" {
		return s.dnsFilePath
	}
	return dnsBookPath()
}

func (s *Server) loadDNSBookLocked() DNSBook {
	raw, err := os.ReadFile(s.effectiveDNSPath())
	if err != nil {
		return defaultDNSBook()
	}
	var b DNSBook
	if json.Unmarshal(raw, &b) != nil {
		return defaultDNSBook()
	}
	return normalizeDNSBook(b)
}

func (s *Server) saveDNSBookLocked(b DNSBook) {
	b = normalizeDNSBook(b)
	s.dnsBook = b
	p := s.effectiveDNSPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if data, err := json.MarshalIndent(b, "", "  "); err == nil {
		tmp := p + ".tmp"
		if os.WriteFile(tmp, data, 0o600) == nil {
			_ = os.Rename(tmp, p)
		}
	}
}

func dnsCreatesCycle(entries []DNSEntry, nodeID, candidateParentID string) bool {
	if candidateParentID == "" || nodeID == "" {
		return false
	}
	if nodeID == candidateParentID {
		return true
	}
	parentOf := make(map[string]string, len(entries))
	for _, e := range entries {
		parentOf[e.ID] = e.ParentID
	}
	cur := candidateParentID
	for depth := 0; depth < len(entries)+2 && cur != ""; depth++ {
		if cur == nodeID {
			return true
		}
		cur = parentOf[cur]
	}
	return false
}

// resolveDNSAddress checks whether rawAddr matches a saved alias (e.g. "btweb://doom.btw" or "doom.btw/about.html")
// in the user's "Minha Lista" DNS book. Returns (resolvedAddress, savedPasswords, displayLabel, matched).
func (s *Server) resolveDNSAddress(rawAddr string) (string, []string, string, bool) {
	trimmed := strings.TrimSpace(rawAddr)
	if trimmed == "" {
		return rawAddr, nil, rawAddr, false
	}
	// Never hijack full HTTP/HTTPS URLs or local disk paths
	low := strings.ToLower(trimmed)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") ||
		strings.Contains(trimmed, `:\`) || strings.HasPrefix(trimmed, "/") {
		return rawAddr, nil, rawAddr, false
	}

	rest, hadScheme := stripOpticalScheme(trimmed)
	host := rest
	subPath := ""
	if idx := strings.IndexByte(rest, '/'); idx >= 0 {
		host = rest[:idx]
		subPath = strings.TrimPrefix(rest[idx+1:], "/")
	}
	hostKey := strings.ToLower(strings.TrimSpace(host))
	if hostKey == "" {
		return rawAddr, nil, rawAddr, false
	}

	s.mu.RLock()
	entries := s.dnsBook.Entries
	var matched *DNSEntry
	for i := range entries {
		e := &entries[i]
		if e.Kind != "site" || strings.TrimSpace(e.Source) == "" {
			continue
		}
		if e.Alias != "" && strings.EqualFold(e.Alias, hostKey) {
			cp := *e
			matched = &cp
			break
		}
	}
	s.mu.RUnlock()

	if matched == nil {
		return rawAddr, nil, rawAddr, false
	}

	target := strings.TrimSpace(matched.Source)
	if subPath != "" {
		// Append subPath cleanly to btweb:// target if applicable
		if _, isOpt := stripOpticalScheme(target); isOpt {
			target = strings.TrimRight(target, "/") + "/" + subPath
		} else if ytID := youtube.ExtractVideoID(target); ytID != "" {
			target = "btweb://" + ytID + "/" + subPath
		}
	}
	disp := "btweb://" + matched.Alias
	if subPath != "" {
		disp += "/" + subPath
	} else if !hadScheme && matched.Alias != "" {
		disp = "btweb://" + matched.Alias
	}
	return target, append([]string(nil), matched.Passwords...), disp, true
}

func (s *Server) handleDNSGet(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	b := s.dnsBook
	s.mu.RUnlock()
	writeJSON(w, b)
}

func (s *Server) handleDNSSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req DNSEntry
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Source = strings.TrimSpace(req.Source)
	req.Alias = normalizeDNSAlias(req.Alias)
	if req.Kind != "folder" && req.Kind != "site" {
		if req.Source != "" {
			req.Kind = "site"
		} else {
			req.Kind = "folder"
		}
	}
	if req.Name == "" {
		if req.Alias != "" {
			req.Name = req.Alias
		} else if req.Source != "" {
			req.Name = req.Source
		} else {
			writeError(w, 400, "informe um nome para o item")
			return
		}
	}
	if req.Kind == "site" && req.Source == "" {
		writeError(w, 400, "informe o link do YouTube, btweb:// ou arquivo de origem do site")
		return
	}
	// Clean empty passwords
	cleanPWs := make([]string, 0, len(req.Passwords))
	for _, pw := range req.Passwords {
		if pw != "" {
			cleanPWs = append(cleanPWs, pw)
		}
	}
	req.Passwords = cleanPWs

	now := time.Now().Format(time.RFC3339)

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.ID != "" && dnsCreatesCycle(s.dnsBook.Entries, req.ID, req.ParentID) {
		writeError(w, 400, "não é possível mover um item para dentro dele mesmo")
		return
	}

	foundIdx := -1
	for i := range s.dnsBook.Entries {
		if s.dnsBook.Entries[i].ID == req.ID && req.ID != "" {
			foundIdx = i
			break
		}
	}

	if foundIdx >= 0 {
		existing := s.dnsBook.Entries[foundIdx]
		if existing.Protected {
			req.Protected = true
			req.Kind = "folder"
			req.ParentID = ""
		}
		req.CreatedAt = existing.CreatedAt
		if req.CreatedAt == "" {
			req.CreatedAt = now
		}
		req.UpdatedAt = now
		if req.Order == 0 && existing.Order != 0 && req.ParentID == existing.ParentID {
			req.Order = existing.Order
		}
		s.dnsBook.Entries[foundIdx] = req
	} else {
		id, _ := crypto.RandomHex(6)
		req.ID = "dns_" + id
		req.Protected = false
		req.CreatedAt = now
		req.UpdatedAt = now
		maxOrd := 0
		for _, e := range s.dnsBook.Entries {
			if e.ParentID == req.ParentID && e.Order >= maxOrd {
				maxOrd = e.Order + 1
			}
		}
		req.Order = maxOrd
		s.dnsBook.Entries = append(s.dnsBook.Entries, req)
	}

	s.saveDNSBookLocked(s.dnsBook)
	writeJSON(w, map[string]any{"ok": true, "entry": req, "book": s.dnsBook})
}

func (s *Server) handleDNSDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		ID             string `json:"id"`
		DeleteChildren bool   `json:"delete_children"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeError(w, 400, "ID inválido")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var target *DNSEntry
	for i := range s.dnsBook.Entries {
		if s.dnsBook.Entries[i].ID == req.ID {
			cp := s.dnsBook.Entries[i]
			target = &cp
			break
		}
	}
	if target == nil {
		writeError(w, 404, "item não encontrado")
		return
	}
	if target.Protected || target.ID == DefaultMySitesFolderID {
		writeError(w, 400, "a pasta 'Meus Sites' é padrão e não pode ser removida")
		return
	}

	toDelete := map[string]bool{target.ID: true}
	if req.DeleteChildren {
		changed := true
		for changed {
			changed = false
			for _, e := range s.dnsBook.Entries {
				if !toDelete[e.ID] && toDelete[e.ParentID] && !e.Protected {
					toDelete[e.ID] = true
					changed = true
				}
			}
		}
	}

	kept := make([]DNSEntry, 0, len(s.dnsBook.Entries))
	for _, e := range s.dnsBook.Entries {
		if toDelete[e.ID] {
			continue
		}
		if e.ParentID == target.ID {
			e.ParentID = target.ParentID // Promote children cleanly so nothing is orphaned!
		}
		kept = append(kept, e)
	}
	s.dnsBook.Entries = kept
	s.saveDNSBookLocked(s.dnsBook)
	writeJSON(w, map[string]any{"ok": true, "book": s.dnsBook})
}

func (s *Server) handleDNSReorder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		ID          string  `json:"id"`
		NewParentID *string `json:"new_parent_id,omitempty"`
		Direction   string  `json:"direction,omitempty"` // "up" | "down"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeError(w, 400, "JSON inválido")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	idx := -1
	for i := range s.dnsBook.Entries {
		if s.dnsBook.Entries[i].ID == req.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, 404, "item não encontrado")
		return
	}

	if req.NewParentID != nil {
		np := strings.TrimSpace(*req.NewParentID)
		if s.dnsBook.Entries[idx].Protected && np != "" {
			writeError(w, 400, "a pasta 'Meus Sites' deve permanecer na raiz de Minha Lista")
			return
		}
		if dnsCreatesCycle(s.dnsBook.Entries, req.ID, np) {
			writeError(w, 400, "não é possível mover um item para dentro dele mesmo ou de um filho seu")
			return
		}
		s.dnsBook.Entries[idx].ParentID = np
		maxOrd := 0
		for _, e := range s.dnsBook.Entries {
			if e.ID != req.ID && e.ParentID == np && e.Order >= maxOrd {
				maxOrd = e.Order + 1
			}
		}
		s.dnsBook.Entries[idx].Order = maxOrd
		s.dnsBook.Entries[idx].UpdatedAt = time.Now().Format(time.RFC3339)
	}

	if req.Direction == "up" || req.Direction == "down" {
		parent := s.dnsBook.Entries[idx].ParentID
		var sibIdxs []int
		for i := range s.dnsBook.Entries {
			if s.dnsBook.Entries[i].ParentID == parent {
				sibIdxs = append(sibIdxs, i)
			}
		}
		sort.SliceStable(sibIdxs, func(a, b int) bool {
			ea, eb := s.dnsBook.Entries[sibIdxs[a]], s.dnsBook.Entries[sibIdxs[b]]
			if ea.Order != eb.Order {
				return ea.Order < eb.Order
			}
			return ea.Name < eb.Name
		})
		pos := -1
		for k, si := range sibIdxs {
			if si == idx {
				pos = k
				break
			}
		}
		if req.Direction == "up" && pos > 0 {
			sibIdxs[pos], sibIdxs[pos-1] = sibIdxs[pos-1], sibIdxs[pos]
		} else if req.Direction == "down" && pos >= 0 && pos < len(sibIdxs)-1 {
			sibIdxs[pos], sibIdxs[pos+1] = sibIdxs[pos+1], sibIdxs[pos]
		}
		for orderVal, si := range sibIdxs {
			s.dnsBook.Entries[si].Order = orderVal
		}
	}

	s.saveDNSBookLocked(s.dnsBook)
	writeJSON(w, map[string]any{"ok": true, "book": s.dnsBook})
}

func (s *Server) handleDNSToggleFavorite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeError(w, 400, "ID inválido")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.dnsBook.Entries {
		if s.dnsBook.Entries[i].ID == req.ID {
			s.dnsBook.Entries[i].Favorite = !s.dnsBook.Entries[i].Favorite
			s.dnsBook.Entries[i].UpdatedAt = time.Now().Format(time.RFC3339)
			s.saveDNSBookLocked(s.dnsBook)
			writeJSON(w, map[string]any{"ok": true, "entry": s.dnsBook.Entries[i], "book": s.dnsBook})
			return
		}
	}
	writeError(w, 404, "item não encontrado")
}

func (s *Server) handleDNSExport(w http.ResponseWriter, r *http.Request) {
	includePWs := r.URL.Query().Get("include_passwords") == "1" || r.URL.Query().Get("include_passwords") == "true"
	s.mu.RLock()
	out := DNSBook{
		Name:    s.dnsBook.Name,
		Version: s.dnsBook.Version,
		Entries: make([]DNSEntry, len(s.dnsBook.Entries)),
	}
	for i, e := range s.dnsBook.Entries {
		cp := e
		if !includePWs {
			cp.Passwords = nil
		}
		out.Entries[i] = cp
	}
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="Minha-Lista-BrightWeb.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func parseImportedDNSPayload(raw []byte) ([]DNSEntry, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, errors.New("arquivo ou link de importação vazio")
	}
	// 1. BrightWeb DNSBook JSON format
	var book DNSBook
	if json.Unmarshal([]byte(trimmed), &book) == nil && len(book.Entries) > 0 {
		return book.Entries, nil
	}
	// 2. Raw JSON array of DNSEntry
	var arr []DNSEntry
	if json.Unmarshal([]byte(trimmed), &arr) == nil && len(arr) > 0 {
		return arr, nil
	}
	// 3. Simple CSV / Google Sheets CSV (columns: name/alias/source/passwords/notes)
	cr := csv.NewReader(strings.NewReader(trimmed))
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err == nil && len(rows) > 0 {
		var out []DNSEntry
		now := time.Now().Format(time.RFC3339)
		startRow := 0
		nameCol, aliasCol, srcCol, pwCol := 0, 1, 2, -1
		hdr := rows[0]
		looksHeader := false
		for ci, col := range hdr {
			lc := strings.ToLower(strings.TrimSpace(col))
			switch lc {
			case "nome", "name", "title", "titulo", "título":
				nameCol = ci
				looksHeader = true
			case "alias", "dominio", "domínio", "domain", "site":
				aliasCol = ci
				looksHeader = true
			case "video", "vídeo", "source", "url", "link", "origem", "address":
				srcCol = ci
				looksHeader = true
			case "senha", "senhas", "password", "passwords":
				pwCol = ci
				looksHeader = true
			}
		}
		if looksHeader {
			startRow = 1
		}
		for ri := startRow; ri < len(rows); ri++ {
			row := rows[ri]
			get := func(c int) string {
				if c >= 0 && c < len(row) {
					return strings.TrimSpace(row[c])
				}
				return ""
			}
			var name, alias, src string
			if looksHeader {
				name = get(nameCol)
				alias = normalizeDNSAlias(get(aliasCol))
				src = get(srcCol)
			} else if len(row) >= 3 {
				name = get(0)
				alias = normalizeDNSAlias(get(1))
				src = get(2)
			} else if len(row) == 2 {
				alias = normalizeDNSAlias(get(0))
				name = get(0)
				src = get(1)
			} else if len(row) == 1 {
				src = get(0)
				name = src
			}
			if src == "" {
				continue
			}
			if name == "" {
				if alias != "" {
					name = alias
				} else {
					name = src
				}
			}
			var pws []string
			if pwStr := get(pwCol); pwStr != "" {
				for _, p := range strings.Split(pwStr, "|") {
					if p = strings.TrimSpace(p); p != "" {
						pws = append(pws, p)
					}
				}
			}
			id, _ := crypto.RandomHex(6)
			out = append(out, DNSEntry{
				ID:        "dns_" + id,
				Kind:      "site",
				Name:      name,
				Alias:     alias,
				Source:    src,
				Passwords: pws,
				Order:     ri,
				CreatedAt: now,
				UpdatedAt: now,
			})
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, errors.New("formato não reconhecido (use JSON exportado do BrightWeb ou CSV/Google Sheets com colunas nome, alias, link)")
}

func rewriteGoogleDocOrSheetExportURL(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if strings.Contains(u, "docs.google.com/spreadsheets/d/") && !strings.Contains(u, "export?format=") {
		parts := strings.SplitN(u, "/spreadsheets/d/", 2)
		if len(parts) == 2 {
			id := strings.SplitN(parts[1], "/", 2)[0]
			if id != "" {
				return "https://docs.google.com/spreadsheets/d/" + id + "/export?format=csv"
			}
		}
	}
	if strings.Contains(u, "docs.google.com/document/d/") && !strings.Contains(u, "export?format=") {
		parts := strings.SplitN(u, "/document/d/", 2)
		if len(parts) == 2 {
			id := strings.SplitN(parts[1], "/", 2)[0]
			if id != "" {
				return "https://docs.google.com/document/d/" + id + "/export?format=txt"
			}
		}
	}
	return u
}

func (s *Server) handleDNSImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		RawContent     string `json:"raw_content"`
		SourceURL      string `json:"source_url"`
		TargetParentID string `json:"target_parent_id"`
		Replace        bool   `json:"replace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}

	var payload []byte
	if strings.TrimSpace(req.SourceURL) != "" {
		fetchURL := rewriteGoogleDocOrSheetExportURL(req.SourceURL)
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Get(fetchURL)
		if err != nil {
			writeError(w, 400, fmt.Sprintf("erro baixando lista da URL: %v", err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			writeError(w, 400, fmt.Sprintf("URL retornou HTTP %d (verifique se está pública)", resp.StatusCode))
			return
		}
		payload, err = io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
	} else {
		payload = []byte(req.RawContent)
	}

	imported, err := parseImportedDNSPayload(payload)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Replace {
		s.dnsBook = defaultDNSBook()
	}

	// Remap imported IDs to avoid collisions while preserving internal folder/site-inside-site hierarchy!
	existingIDs := make(map[string]bool, len(s.dnsBook.Entries))
	existingKeys := make(map[string]bool, len(s.dnsBook.Entries))
	for _, e := range s.dnsBook.Entries {
		existingIDs[e.ID] = true
		if e.Kind == "site" {
			existingKeys[strings.ToLower(e.Alias+"|"+e.Source)] = true
		}
	}

	idRemap := make(map[string]string, len(imported))
	for _, e := range imported {
		if e.ID == DefaultMySitesFolderID {
			idRemap[e.ID] = DefaultMySitesFolderID
			continue
		}
		if e.ID == "" || existingIDs[e.ID] {
			nid, _ := crypto.RandomHex(6)
			idRemap[e.ID] = "dns_" + nid
		} else {
			idRemap[e.ID] = e.ID
		}
	}

	added := 0
	now := time.Now().Format(time.RFC3339)
	for _, e := range imported {
		if e.ID == DefaultMySitesFolderID {
			continue // Don't duplicate default protected folder
		}
		if e.Kind == "site" && existingKeys[strings.ToLower(e.Alias+"|"+e.Source)] {
			continue // Skip exact duplicate site
		}
		newID := idRemap[e.ID]
		if newID == "" {
			nid, _ := crypto.RandomHex(6)
			newID = "dns_" + nid
		}
		newParent := req.TargetParentID
		if e.ParentID != "" {
			if mapped, ok := idRemap[e.ParentID]; ok {
				newParent = mapped
			} else if existingIDs[e.ParentID] {
				newParent = e.ParentID
			}
		}
		e.ID = newID
		e.ParentID = newParent
		e.Protected = false
		e.Alias = normalizeDNSAlias(e.Alias)
		if e.CreatedAt == "" {
			e.CreatedAt = now
		}
		e.UpdatedAt = now
		s.dnsBook.Entries = append(s.dnsBook.Entries, e)
		existingIDs[e.ID] = true
		if e.Kind == "site" {
			existingKeys[strings.ToLower(e.Alias+"|"+e.Source)] = true
		}
		added++
	}

	s.saveDNSBookLocked(s.dnsBook)
	writeJSON(w, map[string]any{"ok": true, "imported_count": added, "book": s.dnsBook})
}

// handleOpenExternal opens an external HTTP/HTTPS link (e.g. YouTube origin video) in the OS default browser.
func (s *Server) handleOpenExternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "método não permitido")
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "JSON inválido")
		return
	}
	u := strings.TrimSpace(req.URL)
	if u == "" {
		writeError(w, 400, "URL vazia")
		return
	}
	// Convert plain 11-char YouTube ID or btweb://ID into clickable YouTube URL
	if rest, isOpt := stripOpticalScheme(u); isOpt {
		first := strings.SplitN(strings.SplitN(rest, "/", 2)[0], "+", 2)[0]
		if ytID := youtube.ExtractVideoID(first); ytID != "" {
			u = "https://www.youtube.com/watch?v=" + ytID
		}
	} else if len(u) == 11 && youtube.ExtractVideoID(u) == u {
		u = "https://www.youtube.com/watch?v=" + u
	}
	low := strings.ToLower(u)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		writeError(w, 400, "apenas links HTTP/HTTPS podem ser abertos no navegador externo")
		return
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	} else if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", u)
	} else {
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
	writeJSON(w, map[string]any{"ok": true, "url": u})
}

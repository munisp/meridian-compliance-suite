package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func subjectOf(r *http.Request) string { return r.Header.Get("X-Subject") }
func roleOf(r *http.Request) string    { return r.Header.Get("X-Role") }

func (s *Service) handleCreateMatter(w http.ResponseWriter, r *http.Request) {
	var m Matter
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&m); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	if m.Title == "" || m.ClientID == "" {
		writeProblem(w, 422, "validation failed", "title and client_id are required")
		return
	}
	m.ID = "mtr-" + ULID()
	if m.Ref == "" {
		m.Ref = s.store.nextRef()
	}
	if m.Status == "" {
		m.Status = "intake"
	}
	if m.PracticeArea == "" {
		m.PracticeArea = "advisory"
	}
	m.OpenedAt = nowRFC3339()
	s.store.PutMatter(&m)
	// seed relations: matter#client and matter#counsel (creator is counsel)
	s.rel.Grant(RelationTuple{Entity: "matter:" + m.ID, Relation: "client", Subject: "user:" + m.ClientID})
	s.rel.Grant(RelationTuple{Entity: "matter:" + m.ID, Relation: "counsel", Subject: subjectOf(r)})
	for _, c := range m.Counsel {
		s.rel.Grant(RelationTuple{Entity: "matter:" + m.ID, Relation: "counsel", Subject: "user:" + c})
	}
	writeJSON(w, 201, m)
}

func (s *Service) handleListMatters(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, 200, map[string]any{"matters": s.store.ListMatters(q.Get("tenant_id"), q.Get("status"), q.Get("client_id"))})
}

func (s *Service) handleGetMatter(w http.ResponseWriter, r *http.Request) {
	m, ok := s.store.GetMatter(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "matter not found")
		return
	}
	if !s.canReadMatter(r, m) {
		writeProblem(w, 403, "forbidden", "no matter#client or matter#counsel relation")
		return
	}
	writeJSON(w, 200, m)
}

func (s *Service) canReadMatter(r *http.Request, m *Matter) bool {
	if roleOf(r) == "admin" || roleOf(r) == "auditor" {
		return true
	}
	return s.rel.Check("matter:"+m.ID, "read", subjectOf(r), s.store)
}

func (s *Service) handleUpdateMatter(w http.ResponseWriter, r *http.Request) {
	m, ok := s.store.GetMatter(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "matter not found")
		return
	}
	if roleOf(r) != "admin" && !s.rel.Check("matter:"+m.ID, "write", subjectOf(r), s.store) {
		writeProblem(w, 403, "forbidden", "matter#counsel relation required")
		return
	}
	var patch struct {
		Title   *string `json:"title"`
		Status  *string `json:"status"`
		Notes   *string `json:"notes"`
		Counsel []string `json:"counsel"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	if patch.Title != nil {
		m.Title = *patch.Title
	}
	if patch.Notes != nil {
		m.Notes = *patch.Notes
	}
	if patch.Status != nil {
		switch *patch.Status {
		case "intake", "active", "on-hold", "closed":
			m.Status = *patch.Status
			if m.Status == "closed" && m.ClosedAt == "" {
				m.ClosedAt = nowRFC3339()
			}
		default:
			writeProblem(w, 422, "validation failed", "invalid status")
			return
		}
	}
	if patch.Counsel != nil {
		m.Counsel = patch.Counsel
		for _, c := range patch.Counsel {
			s.rel.Grant(RelationTuple{Entity: "matter:" + m.ID, Relation: "counsel", Subject: "user:" + c})
		}
	}
	s.store.PutMatter(m)
	writeJSON(w, 200, m)
}

func (s *Service) handleUploadDoc(w http.ResponseWriter, r *http.Request) {
	m, ok := s.store.GetMatter(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "matter not found")
		return
	}
	if roleOf(r) != "admin" && !s.rel.Check("matter:"+m.ID, "write", subjectOf(r), s.store) {
		writeProblem(w, 403, "forbidden", "matter#counsel relation required")
		return
	}
	// multipart upload or JSON {name, content_b64}
	var name, mime string
	var content []byte
	var privileged bool
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeProblem(w, 400, "bad multipart", err.Error())
			return
		}
		name = r.FormValue("name")
		privileged = r.FormValue("privileged") == "true"
		f, fh, err := r.FormFile("file")
		if err != nil {
			writeProblem(w, 422, "file required", err.Error())
			return
		}
		defer f.Close()
		if name == "" {
			name = fh.Filename
		}
		mime = fh.Header.Get("Content-Type")
		content, err = io.ReadAll(io.LimitReader(f, 64<<20))
		if err != nil {
			writeProblem(w, 400, "read failed", err.Error())
			return
		}
	} else {
		var req struct {
			Name       string `json:"name"`
			MimeType   string `json:"mime_type"`
			Privileged bool   `json:"privileged"`
			Content    []byte `json:"content_b64"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
			writeProblem(w, 400, "bad request", err.Error())
			return
		}
		name, mime, privileged, content = req.Name, req.MimeType, req.Privileged, req.Content
	}
	if name == "" || len(content) == 0 {
		writeProblem(w, 422, "validation failed", "name and file content required")
		return
	}
	sum := sha256.Sum256(content)
	d := &Document{
		ID: "doc-" + ULID(), MatterID: m.ID, Name: name, MimeType: orDef(mime, "application/octet-stream"),
		SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(sum[:]),
		Privileged: privileged, UploadedBy: subjectOf(r), UploadedAt: nowRFC3339(),
	}
	if err := s.store.PutDocument(d, content); err != nil {
		writeProblem(w, 500, "store failed", err.Error())
		return
	}
	if privileged {
		// doc#privileged: counsel-only visibility
		s.rel.Grant(RelationTuple{Entity: "doc:" + d.ID, Relation: "privileged", Subject: subjectOf(r)})
	}
	writeJSON(w, 201, d)
}

func (s *Service) handleListDocs(w http.ResponseWriter, r *http.Request) {
	m, ok := s.store.GetMatter(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "matter not found")
		return
	}
	if !s.canReadMatter(r, m) {
		writeProblem(w, 403, "forbidden", "no matter relation")
		return
	}
	isCounsel := roleOf(r) == "admin" || s.rel.Check("matter:"+m.ID, "write", subjectOf(r), s.store)
	docs := s.store.ListDocuments(m.ID, isCounsel)
	writeJSON(w, 200, map[string]any{"documents": docs, "privileged_visible": isCounsel})
}

func (s *Service) handleGetDoc(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.store.GetDocument(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "document not found")
		return
	}
	if roleOf(r) != "admin" && !s.rel.Check("doc:"+d.ID, "read", subjectOf(r), s.store) {
		writeProblem(w, 403, "forbidden", "doc#privileged restricts access")
		return
	}
	writeJSON(w, 200, d)
}

func (s *Service) handleGetDocContent(w http.ResponseWriter, r *http.Request) {
	d, content, ok := s.store.GetDocument(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "document not found")
		return
	}
	if roleOf(r) != "admin" && !s.rel.Check("doc:"+d.ID, "read", subjectOf(r), s.store) {
		writeProblem(w, 403, "forbidden", "doc#privileged restricts access")
		return
	}
	w.Header().Set("Content-Type", d.MimeType)
	w.Header().Set("X-SHA256", d.SHA256)
	w.Write(content)
}

func (s *Service) handleCreateDeadline(w http.ResponseWriter, r *http.Request) {
	m, ok := s.store.GetMatter(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "matter not found")
		return
	}
	if roleOf(r) != "admin" && !s.rel.Check("matter:"+m.ID, "write", subjectOf(r), s.store) {
		writeProblem(w, 403, "forbidden", "matter#counsel relation required")
		return
	}
	var dl Deadline
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&dl); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	if dl.Title == "" || dl.DueAt == "" {
		writeProblem(w, 422, "validation failed", "title and due_at required")
		return
	}
	if _, err := time.Parse(time.RFC3339, dl.DueAt); err != nil {
		writeProblem(w, 422, "validation failed", "due_at must be RFC3339")
		return
	}
	dl.ID = "dl-" + ULID()
	dl.MatterID = m.ID
	dl.Status = "open"
	if dl.Severity == "" {
		dl.Severity = "info"
	}
	dl.CreatedAt = nowRFC3339()
	s.store.PutDeadline(&dl)
	writeJSON(w, 201, dl)
}

func (s *Service) handleListDeadlines(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, 200, map[string]any{"deadlines": s.store.ListDeadlines(q.Get("matter_id"), q.Get("status"), q.Get("due_before"))})
}

func (s *Service) handleUpdateDeadline(w http.ResponseWriter, r *http.Request) {
	dl, ok := s.store.GetDeadline(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "deadline not found")
		return
	}
	m, _ := s.store.GetMatter(dl.MatterID)
	if m != nil && roleOf(r) != "admin" && !s.rel.Check("matter:"+m.ID, "write", subjectOf(r), s.store) {
		writeProblem(w, 403, "forbidden", "matter#counsel relation required")
		return
	}
	var patch struct {
		Status *string `json:"status"`
		DueAt  *string `json:"due_at"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	if patch.Status != nil {
		switch *patch.Status {
		case "open", "met", "missed", "escalated":
			dl.Status = *patch.Status
		default:
			writeProblem(w, 422, "validation failed", "invalid status")
			return
		}
	}
	if patch.DueAt != nil {
		if _, err := time.Parse(time.RFC3339, *patch.DueAt); err != nil {
			writeProblem(w, 422, "validation failed", "due_at must be RFC3339")
			return
		}
		dl.DueAt = *patch.DueAt
	}
	s.store.PutDeadline(dl)
	writeJSON(w, 200, dl)
}

// ---------- client portal ----------

func (s *Service) handlePortalMatters(w http.ResponseWriter, r *http.Request) {
	subj := strings.TrimPrefix(subjectOf(r), "user:")
	matters := s.store.ListMatters("", "", "")
	out := []*Matter{}
	for _, m := range matters {
		if m.ClientID == subj || s.rel.Check("matter:"+m.ID, "read", subjectOf(r), s.store) {
			out = append(out, m)
		}
	}
	writeJSON(w, 200, map[string]any{"matters": out})
}

func (s *Service) handlePortalMatter(w http.ResponseWriter, r *http.Request) {
	m, ok := s.store.GetMatter(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "matter not found")
		return
	}
	subj := strings.TrimPrefix(subjectOf(r), "user:")
	if m.ClientID != subj && !s.rel.Check("matter:"+m.ID, "read", subjectOf(r), s.store) && roleOf(r) != "admin" {
		writeProblem(w, 403, "forbidden", "portal shows your matters only")
		return
	}
	// portal view: non-privileged documents + open deadlines
	docs := s.store.ListDocuments(m.ID, false)
	deadlines := s.store.ListDeadlines(m.ID, "open", "")
	writeJSON(w, 200, map[string]any{"matter": m, "documents": docs, "open_deadlines": deadlines})
}

// ---------- relations ----------

func (s *Service) handleRelCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Entity     string `json:"entity"`
		Permission string `json:"permission"`
		Subject    string `json:"subject"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	allowed := s.rel.Check(req.Entity, req.Permission, req.Subject, s.store)
	writeJSON(w, 200, map[string]any{"allowed": allowed, "checker": "dev-file-backed"})
}

func (s *Service) handleRelList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"tuples": s.rel.Tuples()})
}

func (s *Service) handleRelGrant(w http.ResponseWriter, r *http.Request) {
	var t RelationTuple
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&t); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	s.rel.Grant(t)
	writeJSON(w, 201, t)
}

func (s *Service) handleRelRevoke(w http.ResponseWriter, r *http.Request) {
	var t RelationTuple
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&t); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	s.rel.Revoke(t.Entity, t.Relation, t.Subject)
	writeJSON(w, 200, map[string]string{"status": "revoked"})
}

// ---------- evidence pack ----------

// handleEvidencePack assembles a matter evidence pack (matter + doc hashes +
// deadlines + relation tuples) and stores it in WORM via the core
// audit-evidence API (local fallback). Returns the WORM receipt.
func (s *Service) handleEvidencePack(w http.ResponseWriter, r *http.Request) {
	m, ok := s.store.GetMatter(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "matter not found")
		return
	}
	if roleOf(r) != "admin" && !s.rel.Check("matter:"+m.ID, "write", subjectOf(r), s.store) {
		writeProblem(w, 403, "forbidden", "matter#counsel relation required")
		return
	}
	docs := s.store.ListDocuments(m.ID, true)
	pack := map[string]any{
		"pack_id": "evp-" + ULID(), "assembled_at": nowRFC3339(), "assembled_by": subjectOf(r),
		"matter": m, "deadlines": s.store.ListDeadlines(m.ID, "", ""),
	}
	docMeta := []map[string]any{}
	for _, d := range docs {
		docMeta = append(docMeta, map[string]any{
			"id": d.ID, "name": d.Name, "sha256": d.SHA256, "privileged": d.Privileged,
			"uploaded_by": d.UploadedBy, "uploaded_at": d.UploadedAt,
		})
	}
	pack["documents"] = docMeta
	tuples := []RelationTuple{}
	for _, t := range s.rel.Tuples() {
		if strings.HasSuffix(t.Entity, ":"+m.ID) {
			tuples = append(tuples, t)
		}
	}
	pack["relations"] = tuples
	payload, _ := json.MarshalIndent(pack, "", "  ")
	receipt, err := s.worm.Store("case-evidence-pack", payload, map[string]string{"matter_id": m.ID, "ref": m.Ref})
	if err != nil {
		writeProblem(w, 502, "WORM store failed", err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"evidence_pack": pack, "worm_receipt": receipt})
}

// ---------- deadline watch (escalation + notifications) ----------

func (s *Service) deadlineWatch() {
	ticker := time.NewTicker(s.cfg.WatchEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.scanDeadlines()
		}
	}
}

func (s *Service) scanDeadlines() {
	now := time.Now().UTC()
	warnBefore := now.Add(72 * time.Hour).Format(time.RFC3339)
	for _, dl := range s.store.ListDeadlines("", "open", "") {
		due, err := time.Parse(time.RFC3339, dl.DueAt)
		if err != nil {
			continue
		}
		switch {
		case due.Before(now):
			dl.Status = "missed"
			if !dl.Escalated {
				dl.Escalated = true
				dl.Status = "escalated"
				m, _ := s.store.GetMatter(dl.MatterID)
				to := "registry"
				if m != nil {
					to = m.ClientID
				}
				s.notify.Send("email", to, "DEADLINE MISSED: "+dl.Title,
					"Deadline "+dl.ID+" on matter "+dl.MatterID+" was due "+dl.DueAt+" and is now missed. Escalated to registry.")
			}
			s.store.PutDeadline(dl)
		case dl.DueAt <= warnBefore:
			m, _ := s.store.GetMatter(dl.MatterID)
			to := "counsel"
			if m != nil && len(m.Counsel) > 0 {
				to = m.Counsel[0]
			}
			s.notify.Send("email", to, "Deadline approaching: "+dl.Title,
				"Deadline "+dl.ID+" on matter "+dl.MatterID+" is due "+dl.DueAt+" (<72h).")
			// mark warning once by flipping severity
			if dl.Severity == "info" {
				dl.Severity = "warning"
				s.store.PutDeadline(dl)
			}
		}
	}
}

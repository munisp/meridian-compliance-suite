package main

import (
	"testing"
	"time"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	return &Service{
		cfg:    Config{DataDir: dir, AuthMode: "dev"},
		store:  NewStore(dir),
		rel:    NewRelationChecker(dir + "/relations.json"),
		worm:   &LocalWORM{Dir: dir},
		notify: LogNotifier{},
	}
}

func TestMatterLifecycleAndRelations(t *testing.T) {
	svc := newTestService(t)
	m := &Matter{Title: "VAT appeal", ClientID: "client-1", PracticeArea: "tax-appeal"}
	m.ID = "mtr-1"
	m.Status = "intake"
	m.OpenedAt = nowRFC3339()
	svc.store.PutMatter(m)
	svc.rel.Grant(RelationTuple{Entity: "matter:mtr-1", Relation: "client", Subject: "user:client-1"})
	svc.rel.Grant(RelationTuple{Entity: "matter:mtr-1", Relation: "counsel", Subject: "user:lawyer-1"})

	if !svc.rel.Check("matter:mtr-1", "read", "user:client-1", svc.store) {
		t.Error("client should read own matter")
	}
	if svc.rel.Check("matter:mtr-1", "write", "user:client-1", svc.store) {
		t.Error("client must not write matter (counsel only)")
	}
	if !svc.rel.Check("matter:mtr-1", "write", "user:lawyer-1", svc.store) {
		t.Error("counsel should write matter")
	}
	if svc.rel.Check("matter:mtr-1", "read", "user:stranger", svc.store) {
		t.Error("stranger must not read matter")
	}
}

func TestPrivilegedDocAccess(t *testing.T) {
	svc := newTestService(t)
	m := &Matter{Title: "Audit defence", ClientID: "client-1"}
	m.ID = "mtr-2"
	m.Status = "active"
	m.OpenedAt = nowRFC3339()
	svc.store.PutMatter(m)
	svc.rel.Grant(RelationTuple{Entity: "matter:mtr-2", Relation: "client", Subject: "user:client-1"})
	svc.rel.Grant(RelationTuple{Entity: "matter:mtr-2", Relation: "counsel", Subject: "user:lawyer-1"})

	priv := &Document{ID: "doc-p", MatterID: "mtr-2", Name: "legal-advice.pdf", Privileged: true, SHA256: "abc"}
	svc.store.PutDocument(priv, []byte("secret"))
	pub := &Document{ID: "doc-n", MatterID: "mtr-2", Name: "filing.pdf", Privileged: false, SHA256: "def"}
	svc.store.PutDocument(pub, []byte("public"))

	if svc.rel.Check("doc:doc-p", "read", "user:client-1", svc.store) {
		t.Error("client must not read privileged doc")
	}
	if !svc.rel.Check("doc:doc-p", "read", "user:lawyer-1", svc.store) {
		t.Error("counsel should read privileged doc")
	}
	if !svc.rel.Check("doc:doc-n", "read", "user:client-1", svc.store) {
		t.Error("client should read non-privileged doc")
	}
	// list without privilege hides privileged docs
	docs := svc.store.ListDocuments("mtr-2", false)
	if len(docs) != 1 || docs[0].ID != "doc-n" {
		t.Errorf("non-privileged list = %+v", docs)
	}
}

func TestDeadlineWatchEscalation(t *testing.T) {
	svc := newTestService(t)
	m := &Matter{Title: "TAT filing", ClientID: "client-9"}
	m.ID = "mtr-3"
	m.Status = "active"
	m.OpenedAt = nowRFC3339()
	svc.store.PutMatter(m)
	// overdue deadline
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	dl := &Deadline{ID: "dl-1", MatterID: "mtr-3", Title: "File TAT notice", DueAt: past, Severity: "critical", Status: "open"}
	svc.store.PutDeadline(dl)
	// upcoming deadline (<72h)
	soon := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	dl2 := &Deadline{ID: "dl-2", MatterID: "mtr-3", Title: "Submit bundle", DueAt: soon, Severity: "info", Status: "open"}
	svc.store.PutDeadline(dl2)

	svc.scanDeadlines()
	got, _ := svc.store.GetDeadline("dl-1")
	if got.Status != "escalated" || !got.Escalated {
		t.Errorf("overdue deadline should escalate, got %+v", got)
	}
	got2, _ := svc.store.GetDeadline("dl-2")
	if got2.Severity != "warning" {
		t.Errorf("approaching deadline should warn, got %+v", got2)
	}
}

func TestEvidencePackLocalWORM(t *testing.T) {
	svc := newTestService(t)
	m := &Matter{Title: "Ombud referral", ClientID: "client-2"}
	m.ID = "mtr-4"
	m.Status = "active"
	m.OpenedAt = nowRFC3339()
	svc.store.PutMatter(m)
	svc.store.PutDocument(&Document{ID: "doc-e", MatterID: "mtr-4", Name: "evidence.pdf", SHA256: "ff"}, []byte("data"))

	payload := []byte(`{"matter":"mtr-4"}`)
	receipt, err := svc.worm.Store("case-evidence-pack", payload, map[string]string{"matter_id": "mtr-4"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Source != "local" || receipt.SHA256 == "" || receipt.WORMURI == "" {
		t.Errorf("bad receipt: %+v", receipt)
	}
	// immutability: same payload -> same uri
	r2, _ := svc.worm.Store("case-evidence-pack", payload, nil)
	if r2.WORMURI != receipt.WORMURI {
		t.Error("content-addressed WORM should dedupe")
	}
}

func TestStoreReplay(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	m := &Matter{ID: "mtr-x", Title: "replay test", ClientID: "c", Status: "active", OpenedAt: nowRFC3339()}
	st.PutMatter(m)
	st.PutDeadline(&Deadline{ID: "dl-x", MatterID: "mtr-x", Title: "d", DueAt: "2026-12-31T00:00:00Z", Status: "open"})
	st2 := NewStore(dir)
	if _, ok := st2.GetMatter("mtr-x"); !ok {
		t.Error("matter not replayed")
	}
	if got := st2.ListDeadlines("mtr-x", "", ""); len(got) != 1 {
		t.Error("deadline not replayed")
	}
}

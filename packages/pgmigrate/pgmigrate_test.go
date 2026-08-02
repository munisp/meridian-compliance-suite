package pgmigrate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		file    string
		version int
		name    string
		ok      bool
	}{
		{"0001_einvoicing_uniqueness.sql", 1, "einvoicing_uniqueness", true},
		{"0042_add_tenant_id.sql", 42, "add_tenant_id", true},
		{"README.md", 0, "", false},
		{"no_version.sql", 0, "", false},
		{"0000_zero.sql", 0, "", false},
		{"0001_trailing", 0, "", false},
	}
	for _, c := range cases {
		v, n, ok := ParseVersion(c.file)
		if v != c.version || n != c.name || ok != c.ok {
			t.Errorf("ParseVersion(%q) = (%d,%q,%v), want (%d,%q,%v)",
				c.file, v, n, ok, c.version, c.name, c.ok)
		}
	}
}

func TestLoadOrdersAndIgnoresNonMigrations(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"0003_c.sql", "0001_a.sql", "0002_b.sql", "README.md"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("SELECT 1;"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("want 3 migrations, got %d", len(ms))
	}
	for i, want := range []int{1, 2, 3} {
		if ms[i].Version != want {
			t.Fatalf("ms[%d].Version = %d, want %d", i, ms[i].Version, want)
		}
	}
}

func TestLoadRejectsDuplicateVersions(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"0001_a.sql", "0001_b.sql"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("SELECT 1;"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("want duplicate-version error")
	}
}

// Package pgmigrate is a tiny golang-migrate-style apply-on-boot migration
// runner (no external framework, platform pgx only). Numbered SQL files
// (NNNN_description.sql) in a directory are applied in version order, each
// in its own transaction, and recorded in a schema_migrations table so
// re-boots are no-ops. This gives the platform explicit, reviewable
// migration ordering going forward, replacing startup CREATE TABLE IF NOT
// EXISTS drift.
package pgmigrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration is one parsed NNNN_description.sql file.
type Migration struct {
	Version int
	Name    string
	Path    string
}

// ParseVersion extracts the numeric version from a NNNN_name.sql filename.
// Returns ok=false for files that do not match the convention.
func ParseVersion(filename string) (version int, name string, ok bool) {
	base := filepath.Base(filename)
	if !strings.HasSuffix(base, ".sql") {
		return 0, "", false
	}
	// Companion rollback files (NNNN_name.rollback.sql) are NOT migrations;
	// they are applied manually by operators and must not trip the
	// duplicate-version check for the forward migration they accompany.
	if strings.HasSuffix(base, ".rollback.sql") {
		return 0, "", false
	}
	stem := strings.TrimSuffix(base, ".sql")
	sep := strings.Index(stem, "_")
	if sep <= 0 {
		return 0, "", false
	}
	v, err := strconv.Atoi(stem[:sep])
	if err != nil || v <= 0 {
		return 0, "", false
	}
	return v, stem[sep+1:], true
}

// Load reads dir and returns the migrations sorted by version. Duplicate
// versions are an error (ordering must be unambiguous).
func Load(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migrations dir: %w", err)
	}
	var ms []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		v, name, ok := ParseVersion(e.Name())
		if !ok {
			continue // non-migration files (README etc.) are ignored
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("duplicate migration version %04d (%s and %s)", v, prev, e.Name())
		}
		seen[v] = e.Name()
		ms = append(ms, Migration{Version: v, Name: name, Path: filepath.Join(dir, e.Name())})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	return ms, nil
}

// Apply runs every migration in dir whose version is not yet recorded in
// schema_migrations, in ascending version order, each in a transaction.
// Migration SQL should still be idempotent (IF NOT EXISTS) so a crashed
// boot outside the recorded transaction can be retried safely.
func Apply(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	ms, err := Load(dir)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(
  version int PRIMARY KEY,
  name text NOT NULL,
  applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("schema_migrations ddl: %w", err)
	}
	for _, m := range ms {
		var applied bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, m.Version).Scan(&applied)
		if err != nil {
			return fmt.Errorf("migration %04d status: %w", m.Version, err)
		}
		if applied {
			continue
		}
		sql, err := os.ReadFile(m.Path)
		if err != nil {
			return fmt.Errorf("migration %04d read: %w", m.Version, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("migration %04d begin: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations(version, name) VALUES ($1, $2)`, m.Version, m.Name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %04d record: %w", m.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("migration %04d commit: %w", m.Version, err)
		}
	}
	return nil
}

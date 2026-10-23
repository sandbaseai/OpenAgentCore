package migrations

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Exercise the current development baseline; historical releases are reinstalled,
// not upgraded through the retired migration chain.
func TestComputeProfilesPreserveDefaultSelection(t *testing.T) {
	dsn := os.Getenv("OAC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	if !strings.HasPrefix(cfg.Database, "oac_") || !strings.HasSuffix(cfg.Database, "_tests") {
		t.Fatal("dedicated oac_*_tests database required")
	}
	admin, err := sql.Open("pgx", cfg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "oac_profiles_" + strings.ReplaceAll(uuid.NewString(), "-", "") + "_tests"
	if _, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	cfg.Database = name
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithTableName("oac_schema_version"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.UpTo(t.Context(), 3); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `UPDATE runtime_deployment SET provider_kind='e2b', mode='direct', generation=7, provider_config='{"template":"fixture:build"}', specification='{"resources":{"cpus":2,"memory_mib":2048}}'`); err != nil {
		t.Fatal(err)
	}
	session := uuid.NewString()
	if _, err = db.ExecContext(t.Context(), `INSERT INTO sessions(id,tenant_id,engine,idempotency_key,request_hash,configuration) VALUES($1,$2,'codex','default-session','fixture','{"environment":{"type":"openai_hosted"}}')`, session, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var s string
		if err := db.QueryRowContext(t.Context(), `SELECT (to_jsonb(d)-'compute_profiles')::text FROM runtime_deployment d`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	if _, err = provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if snapshot() != before {
		t.Fatal("migration changed the existing default selection")
	}
	var empty bool
	if err = db.QueryRowContext(t.Context(), `SELECT compute_profiles='{}'::jsonb FROM runtime_deployment`).Scan(&empty); err != nil || !empty {
		t.Fatal("nonempty profile default", empty, err)
	}
	var count int
	if err = db.QueryRowContext(t.Context(), `SELECT count(*) FROM session_compute_generations`).Scan(&count); err != nil || count != 0 {
		t.Fatal("existing Session was assigned a profile", count, err)
	}
	var version int
	if err = db.QueryRowContext(t.Context(), `SELECT max(version_id) FROM oac_schema_version WHERE is_applied`).Scan(&version); err != nil || version != 4 {
		t.Fatal(version, err)
	}
}

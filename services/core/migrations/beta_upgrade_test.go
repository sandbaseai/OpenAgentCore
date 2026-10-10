package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// A dedicated disposable database exercises the real 91-to-97 SQL, never a
// product database. No provider or native sandbox is contacted by this test.
func TestBeta91UpgradePreservesRetainedAllocations(t *testing.T) {
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
	name := "oac_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "") + "_tests"
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
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithTableName("agents_api_schema_version"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.UpTo(t.Context(), 91); err != nil {
		t.Fatal(err)
	}
	installation := uuid.NewString()
	if _, err = db.ExecContext(t.Context(), `UPDATE runtime_deployment SET installation_id=$1,backend_fingerprint=repeat('a',64),provider_kind='e2b',mode='direct',generation=4,web_managed=true,idle_seconds=300,retention_seconds=86400`, installation); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 107; i++ {
		session, environment, device, allocation, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
		if _, err = db.ExecContext(t.Context(), `INSERT INTO sessions(id,tenant_id,engine,idempotency_key,request_hash,configuration) VALUES($1::uuid,$2,'codex',$1::uuid::text,'fixture','{"environment":{"type":"openai_hosted"}}')`, session, tenant); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(t.Context(), `INSERT INTO environments(id,session_id,status) VALUES($1,$2,'disconnected')`, environment, session); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(t.Context(), `INSERT INTO devices(id,tenant_id,name,credential_hash,environment_id) VALUES($1,$2,'fixture',repeat('b',64),$3)`, device, tenant, environment); err != nil {
			t.Fatal(err)
		}
		state := "running"
		if i >= 17 {
			state = "released"
		}
		retained := fmt.Sprintf(`{"protocol_version":"1","current":{"Generation":0,"Name":"allocation-%d","ID":"sandbox-%d","RestoredFrom":null},"retained":{"Reference":"fixture-%d","ID":"sandbox-%d","Data":"{\"version\":1,\"sandbox_id\":\"sandbox-%d\"}","OperationID":"pause-%d","SourceGeneration":0,"SourceName":"allocation-%d","SourceID":"sandbox-%d"},"suspend_id":"pause-%d"}`, i, i, i, i, i, i, i, i, i)
		if _, err = db.ExecContext(t.Context(), `INSERT INTO runtime_allocations(id,environment_id,device_id,provider_key,state,create_settled,compute_phase,compute_state,compute_retained_until,deployment_generation,released_at) VALUES($1,$2,$3,$4,$5,true,'suspended',$6,clock_timestamp()+interval '24 hours',4,CASE WHEN $5='released' THEN clock_timestamp() END)`, allocation, environment, device, installation, state, retained); err != nil {
			t.Fatal(err)
		}
	}
	// Retained bytes as represented by PostgreSQL, bindings, phase and deadlines
	// must be exactly unchanged, including released records.
	snapshot := func() string {
		var v string
		err := db.QueryRowContext(t.Context(), `SELECT jsonb_agg(to_jsonb(a)-'kept_at' ORDER BY id)::text FROM runtime_allocations a`).Scan(&v)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := snapshot()
	if _, err = provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if snapshot() != before {
		t.Fatal("migration changed retained state, ownership or deadline")
	}
	var version, live, workspaces int
	if err = db.QueryRowContext(t.Context(), `SELECT max(version_id) FROM agents_api_schema_version WHERE is_applied`).Scan(&version); err != nil || version != 97 {
		t.Fatal(version, err)
	}
	if err = db.QueryRowContext(t.Context(), `SELECT count(*) FROM runtime_allocations WHERE state<>'released' AND compute_phase='suspended' AND compute_state->>'protocol_version'='1'`).Scan(&live); err != nil || live != 17 {
		t.Fatal(live, err)
	}
	if err = db.QueryRowContext(t.Context(), `SELECT count(*) FROM environment_workspaces`).Scan(&workspaces); err != nil || workspaces != 0 {
		t.Fatal(workspaces, err)
	}
	// Empty workspace ownership permits Down, but it does not restore removed
	// values. Verify the documented E2B-policy rollback incompatibility.
	if _, err = provider.DownTo(t.Context(), 91); err != nil {
		t.Fatal(err)
	}
	var idle, retention int
	if err = db.QueryRowContext(t.Context(), `SELECT idle_seconds,retention_seconds FROM runtime_deployment`).Scan(&idle, &retention); err != nil || idle != 0 || retention != 0 {
		t.Fatal(idle, retention, err)
	}
	if snapshot() != before {
		t.Fatal("down migration changed retained ownership")
	}
}

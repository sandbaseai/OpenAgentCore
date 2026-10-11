package migrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// A dedicated disposable database exercises the real 91-to-99 SQL, never a
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
	if err = db.QueryRowContext(t.Context(), `SELECT max(version_id) FROM agents_api_schema_version WHERE is_applied`).Scan(&version); err != nil || version != 99 {
		t.Fatal(version, err)
	}
	if err = db.QueryRowContext(t.Context(), `SELECT count(*) FROM runtime_allocations WHERE state<>'released' AND compute_phase='suspended' AND compute_state->>'protocol_version'='1'`).Scan(&live); err != nil || live != 17 {
		t.Fatal(live, err)
	}
	if err = db.QueryRowContext(t.Context(), `SELECT count(*) FROM environment_workspaces`).Scan(&workspaces); err != nil || workspaces != 0 {
		t.Fatal(workspaces, err)
	}
	checkBeta99CheckpointConstraint(t, db, installation)
	checkBeta98HistoricalAllocationDowngrade(t, db, provider)
	if snapshot() != before {
		t.Fatal("constraint checks changed retained ownership")
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

// Migration 99 accepts only structured compatibility evidence. It does not
// manufacture a token for existing nodes or reinterpret adapter-private state.
func checkBeta99CheckpointConstraint(t *testing.T, db *sql.DB, installation string) {
	t.Helper()
	node, connection := uuid.NewString(), uuid.NewString()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runtime_nodes(id,installation_id,name,backend_fingerprint,credential_sha256,max_active,max_retained) VALUES($1,$2,'migration-fixture',repeat('c',64),repeat('d',64),1,2)`, node, installation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runtime_node_generation_status(node_id,generation,specification_digest,connection_id,owner_epoch,state) VALUES($1,1,repeat('e',64),$2,1,'ready')`, node, connection); err != nil {
		t.Fatal(err)
	}
	var absent bool
	if err := db.QueryRowContext(t.Context(), `SELECT checkpoint IS NULL FROM runtime_node_generation_status WHERE node_id=$1`, node).Scan(&absent); err != nil || !absent {
		t.Fatal("checkpoint default", absent, err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE runtime_node_generation_status SET checkpoint='{"artifact_domain":"fixture","execution_class":"fixture"}' WHERE node_id=$1`, node); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{`[]`, `"opaque"`, `1`, `null`} {
		_, err := db.ExecContext(t.Context(), `UPDATE runtime_node_generation_status SET checkpoint=$2 WHERE node_id=$1`, node, invalid)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
			t.Fatalf("checkpoint shape %s: %v", invalid, err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM runtime_node_generation_status WHERE node_id=$1`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM runtime_nodes WHERE id=$1`, node); err != nil {
		t.Fatal(err)
	}
}

// Migration 98 keeps multiple historical receipts but one active allocation
// and one live device authority. Down must fail rather than erase history.
func checkBeta98HistoricalAllocationDowngrade(t *testing.T, db *sql.DB, provider *goose.Provider) {
	t.Helper()
	device, allocation := uuid.NewString(), uuid.NewString()
	var environment, tenant, installation string
	if err := db.QueryRowContext(t.Context(), `SELECT a.environment_id,s.tenant_id,a.provider_key FROM runtime_allocations a JOIN environments e ON e.id=a.environment_id JOIN sessions s ON s.id=e.session_id WHERE a.state='running' ORDER BY a.id LIMIT 1`).Scan(&environment, &tenant, &installation); err != nil {
		t.Fatal(err)
	}
	_, err := db.ExecContext(t.Context(), `INSERT INTO devices(id,tenant_id,name,credential_hash,environment_id) VALUES($1,$2,'duplicate-authority',repeat('f',64),$3)`, device, tenant, environment)
	requireBetaUniqueViolation(t, err, "devices_environment_authority")
	if _, err := db.ExecContext(t.Context(), `INSERT INTO devices(id,tenant_id,name,credential_hash,environment_id,revoked_at) VALUES($1,$2,'historical-device',repeat('f',64),$3,clock_timestamp())`, device, tenant, environment); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO runtime_allocations(id,environment_id,device_id,provider_key,state,create_settled,deployment_generation) VALUES($1,$2,$3,$4,'running',true,4)`, allocation, environment, device, installation)
	requireBetaUniqueViolation(t, err, "runtime_allocations_current_environment")
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runtime_allocations(id,environment_id,device_id,provider_key,state,create_settled,released_at,deployment_generation) VALUES($1,$2,$3,$4,'released',true,clock_timestamp(),4)`, allocation, environment, device, installation); err != nil {
		t.Fatal(err)
	}
	_, err = provider.DownTo(t.Context(), 97)
	requireBetaUniqueViolation(t, err, "devices_environment_id_key")
	// Isolate the allocation downgrade guard after proving device history also
	// prevents Down. This fixture never grants the unbound old device authority.
	if _, err := db.ExecContext(t.Context(), `UPDATE devices SET environment_id=NULL WHERE id=$1`, device); err != nil {
		t.Fatal(err)
	}
	_, err = provider.DownTo(t.Context(), 97)
	requireBetaUniqueViolation(t, err, "runtime_allocations_environment_id_key")
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM runtime_allocations WHERE environment_id=$1`, environment).Scan(&count); err != nil || count != 2 {
		t.Fatal("failed downgrade lost history", count, err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM runtime_allocations WHERE id=$1`, allocation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM devices WHERE id=$1`, device); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func requireBetaUniqueViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23505" || pgerr.ConstraintName != constraint {
		t.Fatalf("expected %s uniqueness fence: %v", constraint, err)
	}
}

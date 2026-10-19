package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
)

type artifactObservationStorage struct {
	*fakeArtifactStorage
	outerError error
}

func (s *artifactObservationStorage) WithArtifactStaging(ctx context.Context, key ArtifactStagingKey, stage func(context.Context, ArtifactStagingTx) error) error {
	if err := s.fakeArtifactStorage.WithArtifactStaging(ctx, key, stage); err != nil {
		return err
	}
	return s.outerError
}

type delayedArtifactRead struct {
	io.Reader
	once bool
}

func (r *delayedArtifactRead) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		time.Sleep(30 * time.Millisecond)
	}
	return r.Reader.Read(p)
}

func TestArtifactStagingObservationBoundaries(t *testing.T) {
	for _, mode := range []string{"read", "put", "lock", "stage", "empty", "put_failure", "lock_failure", "stage_failure", "outer_unknown", "unscoped", "foreign_scope"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			sentinel := errors.New("PRIVATE-ERROR-CANARY")
			tx := &fakeStaging{t: t}
			tx.loadEnvironment = returns(Environment{ID: "environment", Configuration: []byte(`{"type":"openai_hosted"}`)})
			tx.putContent = func(r io.Reader, n int64) error {
				if mode == "put" {
					time.Sleep(30 * time.Millisecond)
				}
				if mode == "put_failure" {
					return sentinel
				}
				return discard(r, n)
			}
			tx.lockSession = func() (LockedSession, error) {
				if mode == "lock" {
					time.Sleep(30 * time.Millisecond)
				}
				if mode == "lock_failure" {
					return LockedSession{}, sentinel
				}
				return LockedSession{}, nil
			}
			tx.loadTurn = returns(Turn{ID: "turn", Status: TurnInProgress})
			tx.stageArtifacts = func() error {
				if mode == "stage" {
					time.Sleep(30 * time.Millisecond)
				}
				if mode == "stage_failure" {
					return sentinel
				}
				return nil
			}
			store := &artifactObservationStorage{fakeArtifactStorage: &fakeArtifactStorage{t: t, tx: tx}}
			if mode == "outer_unknown" {
				store.outerError = sentinel
			}
			service, err := NewService(store, nil)
			if err != nil {
				t.Fatal(err)
			}
			data := export(t, nil, exportFile{name: "outputs/PRIVATE-PATH-CANARY", body: "PRIVATE-CONTENT-CANARY"}, exportFile{name: "outputs/second", body: "another"})
			if mode == "empty" {
				data = export(t, nil)
			}
			var body io.Reader = bytes.NewReader(data)
			if mode == "read" {
				body = &delayedArtifactRead{Reader: body}
			}
			ctx := t.Context()
			if mode != "unscoped" {
				ctx = textobs.WithArtifact(ctx, "session", "turn")
			}
			if mode == "foreign_scope" {
				ctx = textobs.WithArtifact(ctx, "another-session", "turn")
			}
			err = service.StageTurnArtifacts(ctx, StageTurnArtifactsCommand{TenantID: "tenant", SessionID: "session", TurnID: "turn", EnvironmentID: "environment", Export: body})
			failed := strings.Contains(mode, "failure") || mode == "outer_unknown"
			if failed && !errors.Is(err, sentinel) || !failed && err != nil {
				t.Fatalf("changed outcome: %v", err)
			}
			if strings.Contains(logs.String(), "PRIVATE-") {
				t.Fatal("private log content")
			}
			var rows []map[string]any
			for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				var row map[string]any
				if json.Unmarshal(line, &row) != nil {
					t.Fatal("bad JSON")
				}
				if row["msg"] == "artifact staging" {
					rows = append(rows, row)
				}
			}
			if mode == "unscoped" || mode == "foreign_scope" {
				if len(rows) != 0 {
					t.Fatal("unexpected scope")
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("summary count %d", len(rows))
			}
			row := rows[0]
			scope, _ := textobs.Artifact(ctx)
			if row["capture_id"] != scope.Capture || row["session_id"] != "session" || row["turn_id"] != "turn" {
				t.Fatal("capture association lost")
			}
			wantStatus, wantConfirmation := "ok", "confirmed"
			if failed {
				wantStatus, wantConfirmation = "error", "unconfirmed"
			}
			if row["status"] != wantStatus || row["confirmation"] != wantConfirmation {
				t.Fatal("incorrect confirmation")
			}
			count := 2
			if mode == "empty" {
				count = 0
			}
			if mode == "put_failure" {
				count = 1
			}
			if row["put_count"] != float64(count) {
				t.Fatal("put count")
			}
			if mode == "put_failure" && (row["lock_session_ms"] != nil || row["load_turn_ms"] != nil || row["stage_artifacts_ms"] != nil) {
				t.Fatal("invented later boundaries")
			}
			if mode == "lock_failure" && (row["load_turn_ms"] != nil || row["stage_artifacts_ms"] != nil) {
				t.Fatal("invented post-lock boundaries")
			}
			if mode == "empty" && row["put_ms"] != float64(0) {
				t.Fatal("empty archive invented Put")
			}
			if field := map[string]string{"read": "read_export_ms", "put": "put_ms", "lock": "lock_session_ms", "stage": "stage_artifacts_ms"}[mode]; field != "" {
				if row[field].(float64) < 25 {
					t.Fatalf("delay missing from %s", field)
				}
			}
			if row["read_export_ms"].(float64) < row["put_ms"].(float64) {
				t.Fatal("Put no longer nested in export read")
			}
			if row["with_staging_ms"].(float64) < row["read_export_ms"].(float64) {
				t.Fatal("wrong outer envelope")
			}
			if mode == "read" && row["put_ms"].(float64) >= row["read_export_ms"].(float64)-20 {
				t.Fatal("header read attributed to Put")
			}
		})
	}
}

package sessionpg

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgtest"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/google/uuid"
)

func TestInputReceiptsExactDurableBatch(t *testing.T) {
	pool := pgtest.Open(t)
	store, service := stagingService(t, pool)
	tenant, session := newSession(t, pool)
	batch := []sessions.Input{{Kind: "message", Payload: json.RawMessage(`{"input":[{"role":"user","content":[{"type":"input_text","text":"secret-first"}]},{"role":"user","content":[{"type":"input_text","text":"secret-second"}]}]}`)}, messageInput("secret-third")}
	original, err := service.SubmitInputs(t.Context(), text(tenant), text(session), "Key", batch)
	if err != nil {
		t.Fatal(err)
	}
	appended := submitMessage(t, service, tenant, session, "key")
	got, err := store.GetInputReceipts(t.Context(), text(tenant), text(session), "Key")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "admitted" || len(got.Receipts) != 2 || len(got.Receipts[0].ItemIDs) != 2 || len(got.Receipts[1].ItemIDs) != 1 {
		t.Fatalf("%+v", got)
	}
	if original[0].TurnID != appended.TurnID {
		t.Fatal("test did not append into same Turn")
	}
	for i, r := range got.Receipts {
		if r.InputIndex != i || r.Sequence != original[i].Sequence || r.TurnID == nil || *r.TurnID != original[i].TurnID {
			t.Fatalf("wrong receipt %+v", r)
		}
		for _, id := range r.ItemIDs {
			var role string
			if err := pool.QueryRow(t.Context(), `SELECT payload->>'role' FROM session_items WHERE session_id=$1 AND id=$2`, session, id).Scan(&role); err != nil || role != "user" {
				t.Fatalf("Item missing %s %v", id, err)
			}
		}
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("content leaked")
	}
	// Concurrent exact retries retain one batch and its Items, even after appends.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := service.SubmitInputs(t.Context(), text(tenant), text(session), "Key", batch); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	fresh, _ := stagingService(t, pgtest.Open(t))
	again, err := fresh.GetInputReceipts(t.Context(), text(tenant), text(session), "Key")
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("unstable retry %+v %v", again, err)
	}
	if _, err := service.SubmitInputs(t.Context(), text(tenant), text(session), "Key", []sessions.Input{messageInput("changed")}); !errors.Is(err, sessions.ErrIdempotencyConflict) {
		t.Fatalf("changed batch: %v", err)
	}
	other, err := store.GetInputReceipts(t.Context(), text(tenant), text(session), "key")
	if err != nil || len(other.Receipts) != 1 || other.Receipts[0].ItemIDs[0] == got.Receipts[0].ItemIDs[0] {
		t.Fatalf("case/append identity %+v %v", other, err)
	}
	for _, lookup := range [][3]string{{uuid.NewString(), text(session), "Key"}, {text(tenant), uuid.NewString(), "Key"}} {
		if _, err := store.GetInputReceipts(t.Context(), lookup[0], lookup[1], lookup[2]); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatalf("isolation %v", err)
		}
	}
	if absent, err := store.GetInputReceipts(t.Context(), text(tenant), text(session), "unknown"); err != nil || absent.Status != "absent" {
		t.Fatalf("unknown key %+v %v", absent, err)
	}
	if _, err := store.GetInputReceipts(t.Context(), text(tenant), text(session), ""); !errors.Is(err, sessions.ErrInvalidInput) {
		t.Fatalf("missing key %v", err)
	}
	exec(t, pool, `UPDATE sessions SET deleted_at=clock_timestamp() WHERE id=$1`, session)
	if _, err := store.GetInputReceipts(t.Context(), text(tenant), text(session), "Key"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("deleted Session: %v", err)
	}
}

func TestInputReceiptsPendingAndFailedReservation(t *testing.T) {
	pool := pgtest.Open(t)
	store, service := stagingService(t, pool)
	tenant, session, _ := newEnvironment(t, pool, "self_hosted", "pending")
	for _, state := range []string{"failed", "expired", "cancelled", "pending"} {
		reservation := reserve(t, service, tenant, session, state)
		if state != "pending" {
			exec(t, pool, `UPDATE environment_input_reservations SET state=$2,settled_at=clock_timestamp() WHERE id=$1`, reservation.ID, state)
		}
		got, err := store.GetInputReceipts(t.Context(), text(tenant), text(session), state)
		want := "failed"
		if state == "pending" {
			want = state
		}
		if err != nil || string(got.Status) != want || got.Receipts == nil || len(got.Receipts) != 0 {
			t.Fatalf("%s %+v %v", state, got, err)
		}
	}
}

func TestInputReceiptsIdleCancellationHasNoItems(t *testing.T) {
	pool := pgtest.Open(t)
	store, service := stagingService(t, pool)
	tenant, session := newSession(t, pool)
	if _, err := submit(t.Context(), service, tenant, session, "cancel", cancelInput); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetInputReceipts(t.Context(), text(tenant), text(session), "cancel")
	if err != nil || len(got.Receipts) != 1 || got.Receipts[0].TurnID != nil || got.Receipts[0].ItemIDs == nil || len(got.Receipts[0].ItemIDs) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

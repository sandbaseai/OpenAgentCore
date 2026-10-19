package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/workspaceobs"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func TestPreparationShutdownAfterOwnershipSettles(t *testing.T) {
	testPreparationObservationAfterOwnershipSettles(t, false)
}

func TestPreparationSuspensionAfterOwnershipSettles(t *testing.T) {
	testPreparationObservationAfterOwnershipSettles(t, true)
}

func testPreparationObservationAfterOwnershipSettles(t *testing.T, suspended bool) {
	for _, state := range []string{"released", "expired", "failed"} {
		for _, window := range []string{"pending_publication", "already_finished", "pruned_pending_publication"} {
			finished := window == "already_finished"
			name := state + "/" + window
			t.Run(name, func(t *testing.T) {
				var logs exportObservationBuffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				defer slog.SetDefault(previous)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cancel()
				timer := time.NewTimer(time.Hour)
				defer timer.Stop()
				timer.Stop()
				observation := workspaceobs.New(workspaceobs.ReceivedContext(ctx, "connection", time.Now()), "3e29a62c-64ef-4cde-8adc-608380e04478", "preparation")
				// Model the exact unlocked state left by releasePreparation or the
				// non-ready prepareExecution branch, before publishPreparation locks.
				p := &preparationState{ctx: ctx, cancel: cancel, timer: timer, observation: observation,
					status: proto.PreparationStatusPayload{Handle: "reader", Revision: 3, State: state}}
				sender := make(workspaceStatusSender, 4)
				r := &Router{sender: sender, shutdownCh: make(chan struct{}), preparations: map[string]*preparationState{"reader": p}}
				if finished {
					observation.Finish(state)
				}
				if window == "pruned_pending_publication" {
					r.mu.Lock()
					r.prunePreparationsLocked()
					r.mu.Unlock()
					if len(r.preparations) != 0 {
						t.Fatal("settled record was not pruned")
					}
				}
				snapshot := p.status
				shutdownCtx, stop := context.WithTimeout(t.Context(), time.Second)
				defer stop()
				request := proto.EnvironmentSuspendPayload{EnvironmentID: "environment", SuspendID: "suspension"}
				if suspended {
					if err := r.Quiesce(shutdownCtx, request); err != nil {
						t.Fatal(err)
					}
				} else if err := r.Shutdown(shutdownCtx); err != nil {
					t.Fatal(err)
				}
				r.publishPreparation(p, snapshot)
				if suspended {
					if err := r.Resume(request, sender); err != nil {
						t.Fatal(err)
					}
					r.mu.Lock()
					r.prunePreparationsLocked()
					r.mu.Unlock()
					if len(r.preparations) != 0 {
						t.Fatal("settled record was not pruned after resume")
					}
				}
				if err := r.Shutdown(shutdownCtx); err != nil {
					t.Fatal(err)
				}
				if p.owns || p.busy || p.status != snapshot || len(sender) != 0 {
					t.Fatal("shutdown changed settled business state or published after close")
				}
				logs.Lock()
				defer logs.Unlock()
				var rows []map[string]any
				for _, line := range bytes.Split(logs.b.Bytes(), []byte("\n")) {
					var row map[string]any
					if json.Unmarshal(line, &row) == nil && row["msg"] == "runtime workspace transport" {
						rows = append(rows, row)
					}
				}
				if len(rows) != 1 {
					t.Fatalf("summaries=%d, want exactly one", len(rows))
				}
				wantOutcome := "shutdown"
				if suspended {
					wantOutcome = "suspended"
				}
				if finished {
					wantOutcome = state
				}
				if rows[0]["outcome"] != wantOutcome || (!finished && rows[0]["snapshot_complete"] != false) {
					t.Fatal(rows[0])
				}
			})
		}
	}
}

func TestPreparationTerminalPublicationOwner(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready_release", true: "busy_release"}[busy], func(t *testing.T) {
			var logs exportObservationBuffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			timer := time.NewTimer(time.Hour)
			defer timer.Stop()
			observation := workspaceobs.New(workspaceobs.ReceivedContext(ctx, "connection", time.Now()), "3e29a62c-64ef-4cde-8adc-608380e04478", "preparation")
			state := "ready"
			if busy {
				state = "preparing"
			}
			p := &preparationState{ctx: ctx, cancel: cancel, timer: timer, observation: observation, owns: true, busy: busy,
				status: proto.PreparationStatusPayload{Handle: "reader", Revision: 1, State: state}}
			sender := make(workspaceStatusSender, 8)
			r := &Router{sender: sender, shutdownCh: make(chan struct{}), preparations: map[string]*preparationState{"reader": p}}
			stale := p.status
			r.releasePreparation(p, "released", "", true)
			// An obsolete publication leaves the current revision's publisher in charge.
			r.publishPreparation(p, stale)
			if busy {
				r.shutdownWG.Add(1)
				r.prepareExecution(p)
			}
			r.shutdownWG.Wait()
			if err := r.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			if p.owns || p.busy || p.status.State != "released" || p.status.Revision != 2 {
				t.Fatal("unexpected preparation state")
			}
			logs.Lock()
			defer logs.Unlock()
			var row map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.b.Bytes()), &row); err != nil {
				t.Fatal(err)
			}
			if row["outcome"] != "released" {
				t.Fatal(row)
			}
		})
	}
}

package dispatch

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func TestWorkspaceExportOwnsPreparationThroughTenMinuteBudget(t *testing.T) {
	for _, interrupt := range []string{"timeout", "cancel", "release", "shutdown"} {
		t.Run(interrupt, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r, sender, request := exporterRouter(t, "normal")
				p := r.preparations[request.Handle]
				p.ctx, p.cancel = context.WithCancel(context.Background())
				defer p.cancel()
				p.deadline = time.Now().Add(5 * time.Minute)
				p.timer = time.AfterFunc(5*time.Minute, func() { r.releasePreparation(p, "expired", "", true) })
				sendExport(t, r, "export-budget", request)
				if got := readExport(t, sender); got.Outcome != "chunk" {
					t.Fatalf("first receipt = %+v", got)
				}
				time.Sleep(6 * time.Minute)
				synctest.Wait()
				r.mu.Lock()
				active, preparationErr := r.workspaceExport != nil, p.ctx.Err()
				r.mu.Unlock()
				if !active || preparationErr != nil {
					t.Fatal("export truncated by old capture or preparation deadline")
				}
				switch interrupt {
				case "timeout":
					time.Sleep(4 * time.Minute)
				case "cancel":
					sendExport(t, r, "export-budget", proto.WorkspaceExportPayload{Step: "cancel"})
				case "release":
					r.releasePreparation(p, "released", "", false)
				case "shutdown":
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if err := r.Shutdown(ctx); err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()
				r.mu.Lock()
				active = r.workspaceExport != nil
				r.mu.Unlock()
				if active {
					t.Fatalf("%s did not settle export", interrupt)
				}
				if p.ctx.Err() == nil {
					t.Fatal("settled export renewed an expired idle preparation")
				}
			})
		})
	}
}

func TestWorkspaceExportCannotReviveExpiredPreparationTimer(t *testing.T) {
	r, sender, request := exporterRouter(t, "normal")
	p := r.preparations[request.Handle]
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer p.cancel()
	started, resume, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	p.timer = time.AfterFunc(0, func() {
		close(started)
		<-resume
		r.releasePreparation(p, "expired", "", false)
		close(stopped)
	})
	<-started
	t.Cleanup(func() { close(resume); <-stopped })
	sendExport(t, r, "expired", request)
	if got := readExport(t, sender); got.Outcome != "rejected" || got.ErrorCode != "resource_unavailable" {
		t.Fatalf("expired preparation accepted: %+v", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.workspaceExport != nil {
		t.Fatal("expired preparation owns export")
	}
}

func TestWorkspaceExportCompletesAfterIdleDeadlineAndConfirmsExpiredRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, sender, request := exporterRouter(t, "normal")
		p := r.preparations[request.Handle]
		p.ctx, p.cancel = context.WithCancel(context.Background())
		defer p.cancel()
		p.requestID = "read-preparation"
		p.status.Handle, p.status.Revision = request.Handle, 1
		p.deadline = time.Now().Add(5 * time.Minute)
		p.timer = time.AfterFunc(5*time.Minute, func() { r.releasePreparation(p, "expired", "", true) })
		sendExport(t, r, "long-success", request)
		receipt := readExport(t, sender)
		if receipt.Outcome != "chunk" {
			t.Fatalf("first receipt=%+v", receipt)
		}
		offset := int64(len(receipt.Data))
		time.Sleep(6 * time.Minute)
		for receipt.Outcome == "chunk" {
			sendExport(t, r, "long-success", proto.WorkspaceExportPayload{Step: "next", Offset: offset})
			receipt = readExport(t, sender)
			offset += int64(len(receipt.Data))
		}
		if receipt.Outcome != "completed" {
			t.Fatalf("long export=%+v", receipt)
		}
		synctest.Wait()
		env := <-sender.replies
		var status proto.PreparationStatusPayload
		if env.Type != proto.TypePreparationStatus || env.DecodePayload(&status) != nil || status.State != "expired" || status.ErrorCode != "" || status.RunID != "" {
			t.Fatalf("expiry=%+v", env)
		}
		release, _ := proto.NewEnvelope(proto.TypeExecutionRelease, p.requestID, proto.ExecutionReleasePayload{Handle: request.Handle})
		if err := r.Handle(t.Context(), release); err != nil {
			t.Fatal(err)
		}
		env = <-sender.replies
		if env.DecodePayload(&status) != nil || status.State != "expired" || status.ErrorCode != "" || status.RunID != "" {
			t.Fatalf("release=%+v", env)
		}
	})
}

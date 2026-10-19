package runtimegateway

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func TestWorkspaceExportTenMinuteBudgetAndEarlierParent(t *testing.T) {
	for _, budget := range []time.Duration{10 * time.Minute, time.Minute} {
		t.Run(budget.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := NewSession(newFakeConn(), "device", "tenant", "test", nil, nil)
				defer s.Close("test")
				ctx, cancel := context.WithTimeout(context.Background(), budget)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					done <- s.ExportWorkspaceOutputs(ctx, proto.WorkspaceExportPayload{Handle: "prepared", EnvironmentID: "env"}, func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err })
				}()
				<-s.sendCh
				time.Sleep(budget - time.Second)
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("export stopped early: %v", err)
				default:
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline = %v", err)
				}
			})
		})
	}
}

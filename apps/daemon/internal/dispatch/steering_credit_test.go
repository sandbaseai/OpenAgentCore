package dispatch_test

import (
	"context"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/agent"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/dispatch"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func TestRejectedSteeringReceiptDoesNotBlockControlLoop(t *testing.T) {
	sender := &shutdownAllSendsBlockSender{entered: make(chan struct{}), terminal: make(chan context.Context, 1), rescue: make(chan struct{})}
	router, err := dispatch.New(dispatch.Config{Registry: agent.NewRegistry(), Sender: sender})
	if err != nil {
		t.Fatal(err)
	}
	defer close(sender.rescue)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// An inactive Run takes the immediate receipt path, without native work.
	handled := make(chan error, 1)
	go func() {
		handled <- router.Handle(ctx, mustEnv(t, proto.TypePromptSteer, "slow", proto.PromptSteerPayload{InputID: "input", Input: proto.TextInput("text")}))
	}()
	select {
	case <-sender.entered:
	case <-ctx.Done():
		t.Fatal("receipt not attempted")
	}
	select {
	case err := <-handled:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("shared control loop blocked on receipt")
	}
	for range 65 {
		if err := router.Handle(ctx, mustEnv(t, proto.TypePromptCancel, "other", proto.PromptCancelPayload{DeliveryID: "cancel-other"})); err != nil {
			t.Fatal(err)
		}
	}
	if len(sender.snapshot()) != 65 {
		t.Fatal("independent control receipts were blocked")
	}
	if err := router.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown did not release credit wait: %v", err)
	}
}

package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/agent"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// Models a sender whose event window has no credit until Core resumes consuming.
type delayedReceiptSender struct {
	entered chan struct{}
	credit  chan struct{}
	result  chan error
}

func (s *delayedReceiptSender) Send(ctx context.Context, _ proto.Envelope) error {
	close(s.entered)
	var err error
	select {
	case <-s.credit:
	case <-ctx.Done():
		err = ctx.Err()
	}
	s.result <- err
	return err
}

func TestSteeringReceiptsSurviveSlowCredit(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "immediate"
		if accepted {
			name = "accepted"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sender := &delayedReceiptSender{entered: make(chan struct{}), credit: make(chan struct{}), result: make(chan error, 1)}
			r, err := New(Config{Registry: agent.NewRegistry(), Sender: sender})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			defer r.Shutdown(context.Background())
			input := proto.PromptSteerPayload{InputID: "input", Input: proto.TextInput("extra")}
			env, err := proto.NewEnvelope(proto.TypePromptSteer, "run", input)
			if err != nil {
				t.Fatal(err)
			}
			published := make(chan bool, 1)
			if accepted {
				state := &sessionState{steering: make(map[string]steeringReceipt)}
				go func() {
					published <- r.publishSteeringReceipt(ctx, state, env, input, [32]byte{}, proto.PromptSteerAckPayload{InputID: input.InputID, Accepted: true})
				}()
			} else if err := r.Handle(ctx, env); err != nil {
				t.Fatal(err)
			}
			select {
			case <-sender.entered:
			case <-ctx.Done():
				t.Fatal("receipt not attempted")
			}
			select {
			case err := <-sender.result:
				t.Fatalf("receipt lost before credit returned: %v", err)
			case <-time.After(5200 * time.Millisecond):
			}
			close(sender.credit)
			select {
			case err := <-sender.result:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("receipt did not resume")
			}
			if accepted {
				select {
				case ok := <-published:
					if !ok {
						t.Fatal("accepted receipt lost")
					}
				case <-ctx.Done():
					t.Fatal("receipt publication did not finish")
				}
			}
		})
	}
}

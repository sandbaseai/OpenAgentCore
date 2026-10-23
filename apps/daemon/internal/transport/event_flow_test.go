package transport

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func flowConn(t *testing.T, runs ...string) *Conn {
	t.Helper()
	c := &Conn{closed: make(chan struct{}), eventCredit: true}
	for _, run := range runs {
		env, _ := proto.NewEnvelope(proto.TypeExecutionStart, "prep", proto.ExecutionStartPayload{RunID: run})
		if _, err := c.handleEventFlow(env); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestExecutionCreditBlocksOnlyItsRun(t *testing.T) {
	c := flowConn(t, "slow", "other")
	event := proto.Envelope{Type: proto.TypeDelta, ID: "slow"}
	for range proto.ExecutionEventWindow {
		if err := reserveCredit(c, context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	blocked := make(chan error, 1)
	go func() { blocked <- reserveCredit(c, ctx, event) }()
	select {
	case err := <-blocked:
		t.Fatalf("window failed to block: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	for _, env := range []proto.Envelope{{Type: proto.TypeHeartbeat}, {Type: proto.TypeInteractionDecisionAck, ID: "slow"}, {Type: proto.TypeDone, ID: "other"}} {
		if err := reserveCredit(c, ctx, env); err != nil {
			t.Fatal(err)
		}
	}
	ack, _ := proto.NewEnvelope(proto.TypeExecutionEventsAck, "slow", proto.ExecutionEventsAckPayload{Count: 1, Bytes: proto.ExecutionEventBytes(event)})
	if _, err := c.handleEventFlow(ack); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("credit did not unblock producer")
	}
	// A duplicate cumulative acknowledgement cannot manufacture another slot.
	if _, err := c.handleEventFlow(ack); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if err := reserveCredit(c, short, event); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("duplicate credited twice: %v", err)
	}
}

func TestExecutionCreditByteBoundAndInvalidAcknowledgement(t *testing.T) {
	c := flowConn(t, "run")
	env := proto.Envelope{Type: proto.TypeDelta, ID: "run"}
	env.Payload = []byte(`""`)
	env.Payload = []byte(`"` + strings.Repeat("x", proto.ExecutionEventWindowBytes-int(proto.ExecutionEventBytes(env))) + `"`)
	if err := reserveCredit(c, context.Background(), env); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := reserveCredit(c, ctx, proto.Envelope{Type: proto.TypeDone, ID: "run"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("byte limit ignored: %v", err)
	}
	ack, _ := proto.NewEnvelope(proto.TypeExecutionEventsAck, "run", proto.ExecutionEventsAckPayload{Count: 2, Bytes: 1})
	if _, err := c.handleEventFlow(ack); !errors.Is(err, errEventCredit) {
		t.Fatalf("future ack accepted: %v", err)
	}
}

func TestExecutionCreditWaitTerminates(t *testing.T) {
	for _, how := range []string{"stop", "close", "cancel"} {
		t.Run(how, func(t *testing.T) {
			c := flowConn(t, "run")
			env := proto.Envelope{Type: proto.TypeDelta, ID: "run"}
			for range proto.ExecutionEventWindow {
				if err := reserveCredit(c, context.Background(), env); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			finished := make(chan error, 1)
			go func() { finished <- reserveCredit(c, ctx, env) }()
			switch how {
			case "stop":
				_, _ = c.handleEventFlow(proto.Envelope{Type: proto.TypeExecutionEventsStop, ID: "run"})
			case "close":
				close(c.closed)
			case "cancel":
				cancel()
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("wait succeeded after termination")
				}
			case <-time.After(time.Second):
				t.Fatal("wait leaked")
			}
		})
	}
}

func reserveCredit(c *Conn, ctx context.Context, env proto.Envelope) error {
	_, err := c.reserveEvent(ctx, env)
	return err
}

func TestSendCancellationRefundsOnlyUnqueuedCredit(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "before_queue"
		capacity := 0
		if queued {
			name = "after_queue"
			capacity = 1
		}
		t.Run(name, func(t *testing.T) {
			c := flowConn(t, "run")
			c.sendCh = make(chan envelopeWithAck, capacity)
			event := proto.Envelope{Type: proto.TypeDelta, ID: "run"}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := c.Send(ctx, event); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("send: %v", err)
			}
			f := c.flows["run"]
			wantCount, wantBytes := uint64(0), uint64(0)
			if queued {
				wantCount = 1
				wantBytes = proto.ExecutionEventBytes(event)
			}
			if f.sent.Count != wantCount || f.sent.Bytes != wantBytes {
				t.Fatalf("reservation count=%d bytes=%d, want count=%d bytes=%d", f.sent.Count, f.sent.Bytes, wantCount, wantBytes)
			}
			// Unqueued cancellation must leave the entire original window usable.
			if !queued {
				for range proto.ExecutionEventWindow {
					if err := reserveCredit(c, context.Background(), event); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestOversizedExecutionEventClosesConnection(t *testing.T) {
	c := flowConn(t, "run")
	env := proto.Envelope{Type: proto.TypeDelta, ID: "run", Payload: []byte(`"` + strings.Repeat("x", proto.MaxFrameBytes) + `"`)}
	if err := c.Send(context.Background(), env); !errors.Is(err, errEventCredit) {
		t.Fatalf("oversized Send: %v", err)
	}
	select {
	case <-c.closed:
	default:
		t.Fatal("oversized event left peer waiting on an open stream")
	}
	if !errors.Is(c.Err(), errEventCredit) {
		t.Fatalf("connection failure: %v", c.Err())
	}
}

// Reasoning is the upstream replacement for thinking; it must consume the same
// negotiated window without starving control receipts on a saturated Run.
func TestReasoningCreditPreservesControlReceipt(t *testing.T) {
	c := flowConn(t, "run")
	event := proto.Envelope{Type: proto.TypeReasoning, ID: "run"}
	for range proto.ExecutionEventWindow {
		if err := reserveCredit(c, t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := reserveCredit(c, ctx, event); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reasoning bypassed credit: %v", err)
	}
	if err := reserveCredit(c, t.Context(), proto.Envelope{Type: proto.TypeInteractionDecisionAck, ID: "run"}); err != nil {
		t.Fatal(err)
	}
}

package runtimegateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func TestDurableSubscriptionOverflowIsExplicitAndIsolated(t *testing.T) {
	s := newTestSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
	s.eventCredit = true
	defer s.Close("test finished")
	sub, err := s.SubscribeDurable("slow")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.SubscribeDurable("other")
	for range 257 {
		s.dispatchToSubscriber(proto.Envelope{ID: "slow", Type: proto.TypeDelta})
	}
	count := 0
	for range sub.Events {
		count++
	}
	if count != 256 || !errors.Is(sub.Err(), ErrSubscriberOverflow) {
		t.Fatalf("count=%d error=%v", count, sub.Err())
	}
	s.dispatchToSubscriber(proto.Envelope{ID: "slow", Type: proto.TypeDone})
	s.dispatchToSubscriber(proto.Envelope{ID: "other", Type: proto.TypeDone})
	if event := <-other.Events; event.Type != proto.TypeDone || other.Err() != nil {
		t.Fatal("overflow affected another run")
	}
}

func TestSubscriptionCloseAndDispatchAreSerialized(t *testing.T) {
	for range 100 {
		s := newTestSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
		s.eventCredit = true
		sub, _ := s.SubscribeDurable("run")
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			for range 64 {
				s.dispatchToSubscriber(proto.Envelope{ID: "run", Type: proto.TypeDelta})
			}
		}()
		go func() { defer wg.Done(); s.Unsubscribe("run") }()
		go func() { defer wg.Done(); s.Close("disconnected") }()
		wg.Wait()
		for range sub.Events {
		}
	}
}

func TestSubscriptionCreditPreservesBurstAndTail(t *testing.T) {
	s := newTestSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
	s.eventCredit = true
	defer s.Close("test finished")
	sub, err := s.SubscribeDurable("run")
	if err != nil {
		t.Fatal(err)
	}
	var observed uint64
	for batch := 0; batch < 6; batch++ {
		for i := 0; i < proto.ExecutionEventWindow; i++ {
			env, _ := proto.NewEnvelope(proto.TypeDelta, "run", proto.DeltaPayload{Sequence: uint64(batch*proto.ExecutionEventWindow + i)})
			s.dispatchToSubscriber(env)
		}
		for range proto.ExecutionEventWindow {
			env, ok := <-sub.Events
			if !ok {
				t.Fatalf("subscription lost: %v", sub.Err())
			}
			var delta proto.DeltaPayload
			if env.DecodePayload(&delta) != nil || delta.Sequence != observed {
				t.Fatalf("unexpected event at %d", observed)
			}
			observed++
			if err := sub.Consumed(context.Background(), env); err != nil {
				t.Fatal(err)
			}
			frame := <-s.sendCh
			var ack proto.ExecutionEventsAckPayload
			if frame.Type != proto.TypeExecutionEventsAck || frame.DecodePayload(&ack) != nil || ack.Count != observed {
				t.Fatal("invalid cumulative credit")
			}
		}
	}
	s.dispatchToSubscriber(proto.Envelope{Type: proto.TypeDone, ID: "run"})
	if env := <-sub.Events; env.Type != proto.TypeDone {
		t.Fatal("Done lost")
	}
	if _, ok := <-sub.Events; ok || sub.Err() != nil {
		t.Fatalf("stream did not finish: %v", sub.Err())
	}
}

func TestSubscriptionRejectsByteWindowViolation(t *testing.T) {
	s := newTestSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
	s.eventCredit = true
	defer s.Close("test")
	sub, _ := s.SubscribeDurable("run")
	env := proto.Envelope{Type: proto.TypeDelta, ID: "run"}
	env.Payload = []byte(`""`)
	env.Payload = []byte(`"` + strings.Repeat("x", proto.ExecutionEventWindowBytes-int(proto.ExecutionEventBytes(env))) + `"`)
	s.dispatchToSubscriber(env)
	s.dispatchToSubscriber(proto.Envelope{Type: proto.TypeDone, ID: "run"})
	for range sub.Events {
	}
	if sub.FailureReason() != "event_window_exceeded" {
		t.Fatalf("missing byte failure: %v", sub.Err())
	}
}

func TestConnectionCloseReasonDoesNotExposeProse(t *testing.T) {
	for _, reason := range []string{"peer reflected secret", "write error: secret", "owner lease lost to a newer connection"} {
		got := connectionCloseReason(reason)
		if strings.Contains(got, "secret") || got == reason {
			t.Fatalf("unsafe reason: %q", got)
		}
	}
}

func TestReceivedDoneDoesNotRequireLiveCreditTransport(t *testing.T) {
	s := newTestSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
	s.eventCredit = true
	sub, _ := s.SubscribeDurable("run")
	s.dispatchToSubscriber(proto.Envelope{Type: proto.TypeDelta, ID: "run"})
	s.dispatchToSubscriber(proto.Envelope{Type: proto.TypeDone, ID: "run"})
	s.Close("disconnected after terminal")
	var types []string
	for env := range sub.Events {
		types = append(types, env.Type)
		if err := sub.Consumed(context.Background(), env); err != nil {
			t.Fatalf("complete buffered stream required more credit: %v", err)
		}
	}
	if len(types) != 2 || types[1] != proto.TypeDone || sub.Err() != nil {
		t.Fatalf("complete stream lost: %v %v", types, sub.Err())
	}
}

func TestBaseProtocolConsumerNeverSendsCredit(t *testing.T) {
	s := newTestSession(newFakeConn(), "old-runtime", "tenant", "0.16.0", NewRegistry(), nil)
	defer s.Close("test")
	sub, err := s.SubscribeDurable("run")
	if err != nil {
		t.Fatal(err)
	}
	// An old peer never reserves a byte window; retain its existing frame/channel limits.
	env := proto.Envelope{Type: proto.TypeDelta, ID: "run", Payload: []byte(`"` + strings.Repeat("x", proto.MaxFrameBytes/2) + `"`)}
	s.dispatchToSubscriber(env)
	s.dispatchToSubscriber(env)
	for range 2 {
		if err := sub.Consumed(t.Context(), <-sub.Events); err != nil {
			t.Fatal(err)
		}
	}
	for range 300 {
		s.dispatchToSubscriber(proto.Envelope{Type: proto.TypeDelta, ID: "run"})
		if err := sub.Consumed(t.Context(), <-sub.Events); err != nil {
			t.Fatal(err)
		}
	}
	s.dispatchToSubscriber(proto.Envelope{Type: proto.TypeDone, ID: "run"})
	if event := <-sub.Events; event.Type != proto.TypeDone || sub.Err() != nil {
		t.Fatal("base stream lost")
	}
	sub.Stop()
	if len(s.sendCh) != 0 {
		t.Fatal("new control sent to an old Runtime")
	}
}

// Withdrawal is an execution event: it must reach Core before Done and return
// transport credit just like the corresponding function call.
func TestFunctionWithdrawalDispatchesWithAndWithoutCredit(t *testing.T) {
	for _, credit := range []bool{false, true} {
		t.Run(map[bool]string{false: "base", true: "credit"}[credit], func(t *testing.T) {
			s := newTestSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
			s.eventCredit = credit
			defer s.Close("test")
			sub, err := s.SubscribeDurable("run")
			if err != nil {
				t.Fatal(err)
			}
			env, err := proto.NewEnvelope(proto.TypeFunctionCallWithdrawn, "run", proto.FunctionCallWithdrawnPayload{CallID: "call", NativeID: "child", TurnID: "child-turn"})
			if err != nil {
				t.Fatal(err)
			}
			s.dispatch(env)
			select {
			case got := <-sub.Events:
				if got.Type != env.Type || string(got.Payload) != string(env.Payload) {
					t.Fatal("withdrawal identity changed", got)
				}
				if err := sub.Consumed(t.Context(), got); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("function withdrawal was dropped")
			}
			select {
			case frame := <-s.sendCh:
				var ack proto.ExecutionEventsAckPayload
				if !credit || frame.Type != proto.TypeExecutionEventsAck || frame.DecodePayload(&ack) != nil || ack.Count != 1 || ack.Bytes != proto.ExecutionEventBytes(env) {
					t.Fatal("incorrect withdrawal credit", frame)
				}
			default:
				if credit {
					t.Fatal("withdrawal credit was not returned")
				}
			}
			s.dispatch(proto.Envelope{Type: proto.TypeDone, ID: "run"})
			if got := <-sub.Events; got.Type != proto.TypeDone {
				t.Fatal("Done ordering lost", got)
			}
			if _, open := <-sub.Events; open || sub.Err() != nil {
				t.Fatal("terminal event did not close subscription", sub.Err())
			}
		})
	}
}

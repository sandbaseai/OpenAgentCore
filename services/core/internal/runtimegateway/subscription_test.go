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
	s := NewSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
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
		s := NewSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
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
	s := NewSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
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
	s := NewSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
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
	s := NewSession(newFakeConn(), "device", "tenant", proto.Version, NewRegistry(), nil)
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

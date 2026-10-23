package runtimegateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func exportReply(t *testing.T, s *Session, id string, result proto.WorkspaceExportResultPayload) {
	t.Helper()
	env, err := proto.NewEnvelope(proto.TypeWorkspaceExportResult, id, result)
	if err != nil {
		t.Fatal(err)
	}
	s.dispatch(env)
}

// exportSerialFixtureReader models the previous consumer-driven pull schedule
// for a reproducible performance baseline; it is not a production alternative.
type exportSerialFixtureReader struct {
	ctx       context.Context
	peer      *Session
	id        string
	replies   <-chan proto.Envelope
	offset    int64
	data      []byte
	completed bool
}

func (r *exportSerialFixtureReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 && !r.completed {
		request := proto.WorkspaceExportPayload{Step: "next", Offset: r.offset}
		if r.offset == 0 {
			request = proto.WorkspaceExportPayload{Step: "begin", Handle: "prepared", EnvironmentID: "env"}
		}
		env, err := proto.NewEnvelope(proto.TypeWorkspaceExport, r.id, request)
		if err != nil {
			return 0, err
		}
		if err := r.peer.Send(r.ctx, env); err != nil {
			return 0, err
		}
		var result proto.WorkspaceExportResultPayload
		select {
		case env := <-r.replies:
			if err := env.DecodePayload(&result); err != nil {
				return 0, err
			}
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
		r.completed, r.data = result.Outcome == "completed", result.Data
		r.offset += int64(len(result.Data))
	}
	if r.completed {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

type exportSlowHashWriter struct {
	hash  io.Writer
	delay time.Duration
}

func (w exportSlowHashWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.hash.Write(p)
}

func TestWorkspaceExportRoundTripFixture(t *testing.T) {
	body := make([]byte, 8<<20)
	for i := range body {
		body[i] = byte(i * 37)
	}
	want := sha256.Sum256(body)
	for _, test := range []struct {
		name     string
		chunk    int
		baseline bool
	}{
		{"serial_short_reads", 16 << 10, true},
		{"serial_full_chunks", proto.WorkspaceExportChunkBytes, true},
		{"coalesced_prefetch", proto.WorkspaceExportChunkBytes, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestSession(newFakeConn(), "device", "tenant", "test", nil, nil)
			defer s.Close("test")
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			replies := make(chan proto.Envelope, 1)
			peerDone := make(chan int, 1)
			go func() {
				count := 0
				for {
					var env proto.Envelope
					select {
					case frame := <-s.sendCh:
						env = frame.Envelope
					case <-ctx.Done():
						peerDone <- count
						return
					}
					var request proto.WorkspaceExportPayload
					if env.DecodePayload(&request) != nil || request.Step == "cancel" {
						peerDone <- count
						return
					}
					count++
					time.Sleep(2 * time.Millisecond) // Controlled request/reply RTT.
					result := proto.WorkspaceExportResultPayload{Outcome: "completed", Offset: request.Offset}
					if request.Offset < int64(len(body)) {
						end := min(int(request.Offset)+test.chunk, len(body))
						result.Outcome, result.Data = "chunk", body[int(request.Offset):end]
					}
					response, _ := proto.NewEnvelope(proto.TypeWorkspaceExportResult, env.ID, result)
					if test.baseline {
						replies <- response
					} else {
						s.dispatch(response)
					}
					if result.Outcome == "completed" {
						peerDone <- count
						return
					}
				}
			}()
			got := sha256.New()
			consume := func(r io.Reader) error {
				_, err := io.CopyBuffer(exportSlowHashWriter{got, time.Millisecond}, r, make([]byte, proto.WorkspaceExportChunkBytes))
				return err
			}
			started := time.Now()
			var err error
			if test.baseline {
				err = consume(&exportSerialFixtureReader{ctx: ctx, peer: s, id: "baseline", replies: replies})
			} else {
				err = s.ExportWorkspaceOutputs(ctx, proto.WorkspaceExportPayload{Handle: "prepared", EnvironmentID: "env"}, consume)
			}
			elapsed := time.Since(started)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Sum(nil), want[:]) {
				t.Fatal("fixture SHA-256 mismatch")
			}
			rounds := <-peerDone
			if rounds != len(body)/test.chunk+1 {
				t.Fatal("wrong network round count", rounds)
			}
			t.Logf("fixture=8MiB RTT=2ms consumer=1ms rounds=%d elapsed=%s sha256=%s", rounds, elapsed.Round(time.Millisecond), fmt.Sprintf("%x", got.Sum(nil)))
		})
	}
}

func TestWorkspaceExportPrefetchesOneReplyAndRequiresCompletion(t *testing.T) {
	s := newTestSession(newFakeConn(), "device", "tenant", "test", nil, nil)
	defer s.Close("test")
	consumed, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	body := bytes.Repeat([]byte{0, 255, 7}, 100)
	go func() {
		done <- s.ExportWorkspaceOutputs(t.Context(), proto.WorkspaceExportPayload{Handle: "prepared", EnvironmentID: "env"}, func(r io.Reader) error {
			got := make([]byte, len(body))
			if _, err := io.ReadFull(r, got); err != nil {
				return err
			}
			if !bytes.Equal(got, body) {
				return errors.New("bytes differ")
			}
			close(consumed)
			<-resume
			return nil
		})
	}()
	first := <-s.sendCh
	exportReply(t, s, "foreign", proto.WorkspaceExportResultPayload{Outcome: "completed"})
	exportReply(t, s, first.ID, proto.WorkspaceExportResultPayload{Outcome: "chunk", Data: body})
	<-consumed
	next := <-s.sendCh
	var request proto.WorkspaceExportPayload
	if next.DecodePayload(&request) != nil || request.Step != "next" || request.Offset != int64(len(body)) {
		t.Fatal("bad continuation", request)
	}
	select {
	case err := <-done:
		t.Fatal("succeeded before native completion", err)
	default:
	}
	exportReply(t, s, next.ID, proto.WorkspaceExportResultPayload{Outcome: "completed", Offset: int64(len(body))})
	select {
	case env := <-s.sendCh:
		t.Fatalf("more than one prefetched request: %s", env.Type)
	default:
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceExportConsumerFailureCancelsPendingPrefetch(t *testing.T) {
	s := newTestSession(newFakeConn(), "device", "tenant", "test", nil, nil)
	defer s.Close("test")
	want := errors.New("consumer failed")
	done := make(chan error, 1)
	go func() {
		done <- s.ExportWorkspaceOutputs(t.Context(), proto.WorkspaceExportPayload{Handle: "prepared", EnvironmentID: "env"}, func(r io.Reader) error {
			if _, err := r.Read(make([]byte, 1)); err != nil {
				return err
			}
			return want
		})
	}()
	first := <-s.sendCh
	exportReply(t, s, first.ID, proto.WorkspaceExportResultPayload{Outcome: "chunk", Data: []byte("bytes")})
	next := <-s.sendCh
	var request proto.WorkspaceExportPayload
	if next.DecodePayload(&request) != nil || request.Step != "next" || request.Offset != 5 {
		t.Fatal("missing prefetch")
	}
	if err := <-done; !errors.Is(err, want) {
		t.Fatal("consumer error lost", err)
	}
	cleanup := <-s.sendCh
	if cleanup.DecodePayload(&request) != nil || request.Step != "cancel" {
		t.Fatal("pending prefetch not cancelled")
	}
	// A late receipt from the cancelled owner must not retain capacity.
	exportReply(t, s, first.ID, proto.WorkspaceExportResultPayload{Outcome: "chunk", Offset: 5, Data: []byte("late")})
	s.workspaceExportMu.Lock()
	defer s.workspaceExportMu.Unlock()
	if len(s.workspaceExports) != 0 {
		t.Fatal("consumer failure retained export")
	}
}

func TestWorkspaceExportRejectsInvalidReceiptScopeAndMaximum(t *testing.T) {
	for _, test := range []struct {
		name    string
		offset  int64
		id, typ string
		result  proto.WorkspaceExportResultPayload
	}{
		{"foreign id", 0, "foreign", proto.TypeWorkspaceExportResult, proto.WorkspaceExportResultPayload{Outcome: "completed"}},
		{"foreign type", 0, "export", proto.TypeWorkspaceReadResult, proto.WorkspaceExportResultPayload{Outcome: "completed"}},
		{"out of order", 1, "export", proto.TypeWorkspaceExportResult, proto.WorkspaceExportResultPayload{Outcome: "completed"}},
		{"overflow", proto.WorkspaceExportMaxBytes, "export", proto.TypeWorkspaceExportResult, proto.WorkspaceExportResultPayload{Outcome: "chunk", Offset: proto.WorkspaceExportMaxBytes, Data: []byte("x")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestSession(newFakeConn(), "device", "tenant", "test", nil, nil)
			defer s.Close("test")
			replies := make(chan workspaceExportReceipt, 1)
			env, err := proto.NewEnvelope(test.typ, test.id, test.result)
			if err != nil {
				t.Fatal(err)
			}
			replies <- workspaceExportReceipt{Envelope: env, dispatched: time.Now()}
			r := &workspaceExportReader{ctx: t.Context(), peer: s, id: "export", replies: replies, offset: test.offset, pending: true}
			if _, err := r.Read(make([]byte, 1)); err == nil {
				t.Fatal("invalid receipt accepted")
			}
		})
	}
}

func TestWorkspaceExportPrefetchStaysOneChunkAheadOfSlowConsumer(t *testing.T) {
	s := newTestSession(newFakeConn(), "device", "tenant", "test", nil, nil)
	defer s.Close("test")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	consumed, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.ExportWorkspaceOutputs(ctx, proto.WorkspaceExportPayload{Handle: "prepared", EnvironmentID: "env"}, func(r io.Reader) error {
			_, err := r.Read(make([]byte, 1))
			close(consumed)
			<-resume
			_, err = r.Read(make([]byte, 1))
			return err
		})
	}()
	first := <-s.sendCh
	exportReply(t, s, first.ID, proto.WorkspaceExportResultPayload{Outcome: "chunk", Data: make([]byte, proto.WorkspaceExportChunkBytes)})
	<-consumed
	<-s.sendCh
	exportReply(t, s, first.ID, proto.WorkspaceExportResultPayload{Outcome: "chunk", Offset: proto.WorkspaceExportChunkBytes, Data: make([]byte, proto.WorkspaceExportChunkBytes)})
	select {
	case <-s.sendCh:
		t.Fatal("unbounded prefetch")
	default:
	}
	cancel()
	close(resume)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled buffered read succeeded", err)
	}
}

func TestWorkspaceExportRejectsInvalidChunksAndNativeFailure(t *testing.T) {
	for _, result := range []proto.WorkspaceExportResultPayload{
		{Outcome: "chunk", Offset: 1, Data: []byte("x")},
		{Outcome: "chunk", Data: make([]byte, proto.WorkspaceExportChunkBytes+1)},
		{Outcome: "chunk"},
		{Outcome: "completed", Data: []byte("x")},
		{Outcome: "failed", ErrorCode: "export_failed"},
	} {
		s := newTestSession(newFakeConn(), "device", "tenant", "test", nil, nil)
		done := make(chan error, 1)
		go func() {
			done <- s.ExportWorkspaceOutputs(t.Context(), proto.WorkspaceExportPayload{Handle: "prepared", EnvironmentID: "env"}, func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err })
		}()
		first := <-s.sendCh
		exportReply(t, s, first.ID, result)
		if err := <-done; err == nil {
			t.Fatal("invalid export accepted", result.Outcome)
		}
		cancel := <-s.sendCh
		var request proto.WorkspaceExportPayload
		if cancel.DecodePayload(&request) != nil || request.Step != "cancel" {
			t.Fatal("missing cleanup request")
		}
		s.Close("test")
	}
}

func TestWorkspaceExportDisconnectAndCancellationSettleRead(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		s := newTestSession(newFakeConn(), "device", "tenant", "test", nil, nil)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- s.ExportWorkspaceOutputs(ctx, proto.WorkspaceExportPayload{Handle: "prepared", EnvironmentID: "env"}, func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err })
		}()
		<-s.sendCh
		if disconnect {
			s.Close("lost")
		} else {
			cancel()
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("interrupted export succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("read did not settle")
		}
		cancel()
		s.Close("test")
	}
}

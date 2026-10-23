package runtimegateway

import (
	"bytes"
	"encoding/json"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPredecodeRealReadLoop(t *testing.T) {
	c := newFakeConn()
	s := newTestSession(c, "device", "tenant", "test", nil, nil)
	defer s.Close("test")
	ctx := textobs.WithArtifact(t.Context(), "5889faac-763b-4a6b-bae6-e809fe65e0cd", "4fbfe7d7-cfdb-4b96-917f-79b3d251c8b5")
	finish := s.ObserveArtifactTransport(ctx, "prepare")
	defer finish()
	s.observeArtifactExport(ctx, "export")
	s.Start()
	env, _ := proto.NewEnvelope(proto.TypeWorkspaceExportResult, "export", proto.WorkspaceExportResultPayload{Outcome: "completed"})
	raw, _ := json.Marshal(env)
	c.Feed(raw)
	o := s.artifactFor(env)
	deadline := time.Now().Add(time.Second)
	for {
		o.mu.Lock()
		m := o.stages[artifactPredecode]
		o.mu.Unlock()
		if m.count == 1 {
			if m.total < 0 {
				t.Fatal(m)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no read")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestExportRoundBoundAndMissingReceipt(t *testing.T) {
	r := &workspaceExportReader{observation: &workspaceExportObservation{}}
	for i := 0; i < 12; i++ {
		start := time.Now()
		r.dispatched = time.Now()
		r.observeRound(start, proto.WorkspaceExportPayload{Step: "next", Offset: int64(i)}, "chunk")
	}
	o := r.observation
	if len(o.rounds) != 8 || o.roundCount != 12 || !o.roundsTruncated || o.roundMetric.count != 12 {
		t.Fatal(o)
	}
	r.dispatched = time.Time{}
	r.observeRound(time.Now(), proto.WorkspaceExportPayload{Step: "next"}, "unknown")
	if o.roundMetric.unknown != 1 {
		t.Fatal(o)
	}
	o.roundCount = 1000000
	r.observeRound(time.Now(), proto.WorkspaceExportPayload{Step: "next"}, "unknown")
	if o.roundCount != 1000000 || !o.roundMetric.overflow {
		t.Fatal(o)
	}
}
func TestC2CoreProducerFixtures(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			var h slog.Handler = slog.NewJSONHandler(&buf, nil)
			if format == "text" {
				h = slog.NewTextHandler(&buf, nil)
			}
			old := slog.Default()
			slog.SetDefault(slog.New(h))
			defer slog.SetDefault(old)
			ctx := textobs.WithArtifact(t.Context(), "5889faac-763b-4a6b-bae6-e809fe65e0cd", "4fbfe7d7-cfdb-4b96-917f-79b3d251c8b5")
			scope, _ := textobs.Artifact(ctx)
			r := &workspaceExportReader{ctx: ctx, id: "378281c9-a535-412a-b217-45520991855e", observation: &workspaceExportObservation{started: time.Now()}}
			start := time.Now()
			r.dispatched = time.Now()
			r.observeRound(start, proto.WorkspaceExportPayload{Step: "begin"}, "completed")
			r.observe(scope, nil)
			s := newTestSession(newFakeConn(), "device", "tenant", "test", nil, nil)
			defer s.Close("test")
			finish := s.ObserveArtifactTransport(ctx, "29117c12-a502-4d69-9ad5-27848a6b92ea")
			s.observeArtifactExport(ctx, "378281c9-a535-412a-b217-45520991855e")
			finish()
			if directory := os.Getenv("B279_C2_FIXTURE_DIR"); directory != "" {
				if err := os.WriteFile(filepath.Join(directory, "core-"+format+".log"), buf.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestReceiptCanPrecedeWriteReturn(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	c := &artifactSlowConn{fakeConn: newFakeConn(), entered: make(chan struct{}), release: make(chan struct{})}
	s := newTestSession(c, "device", "tenant", "test", nil, nil)
	defer s.Close("test")
	s.Start()
	ctx := textobs.WithArtifact(t.Context(), "5889faac-763b-4a6b-bae6-e809fe65e0cd", "4fbfe7d7-cfdb-4b96-917f-79b3d251c8b5")
	done := make(chan error, 1)
	go func() {
		done <- s.ExportWorkspaceOutputs(ctx, proto.WorkspaceExportPayload{Handle: "canary-handle", EnvironmentID: "canary-environment"}, func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err })
	}()
	<-c.entered
	s.workspaceExportMu.Lock()
	var id string
	for k := range s.workspaceExports {
		id = k
	}
	s.workspaceExportMu.Unlock()
	reply, _ := proto.NewEnvelope(proto.TypeWorkspaceExportResult, id, proto.WorkspaceExportResultPayload{Outcome: "completed"})
	raw, _ := json.Marshal(reply)
	c.Feed(raw)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(c.release)
		t.Fatal("receipt waited for write return")
	}
	close(c.release)
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
		t.Fatal(err)
	}
	if row["request_send_to_receipt_dispatch_count"] != float64(1) || row["request_send_to_receipt_dispatch_unknown_count"] != float64(0) {
		t.Fatal(row)
	}
	if _, ok := row["write_return_to_receipt_dispatch_ms"]; ok {
		t.Fatal("unmeasured optional boundary published")
	}
}

func TestArtifactMetricBounds(t *testing.T) {
	m := artifactTransportStage{}
	m.add(-time.Nanosecond, false)
	m.add(31*time.Minute, false)
	if m.count != 0 || m.unknown != 2 || !m.overflow {
		t.Fatal(m)
	}
	m.count = 1000000
	m.add(time.Millisecond, false)
	if m.count != 1000000 || m.unknown != 3 {
		t.Fatal(m)
	}
}

func TestC2UnknownCountersSaturate(t *testing.T) {
	m := artifactTransportStage{unknown: 1000000, active: 1}
	m.addUnknown(m.active)
	if m.unknown != 1000000 || !m.overflow {
		t.Fatal(m)
	}
	r := &workspaceExportReader{observation: &workspaceExportObservation{roundMetric: artifactTransportStage{unknown: 1000000}}}
	r.observeRound(time.Now(), proto.WorkspaceExportPayload{Step: "begin"}, "unknown")
	if r.observation.roundMetric.unknown != 1000000 || !r.observation.roundMetric.overflow {
		t.Fatal(r.observation.roundMetric)
	}
	o := &artifactTransport{}
	o.stages[artifactQueue].unknown = 1000000
	o.queue(0, 1)
	if o.stages[artifactQueue].unknown != 1000000 || !o.stages[artifactQueue].overflow {
		t.Fatal(o.stages[artifactQueue])
	}
}

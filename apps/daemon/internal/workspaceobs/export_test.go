package workspaceobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/localworkspace"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriterActualProducerBackpressureAndActiveEnvelope(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "slow_reader", true: "active_work"}[active], func(t *testing.T) {
			binding, err := localworkspace.NewWithCapabilityDirectory(uuid.NewString(), uuid.NewString(), t.TempDir(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := New(t.Context(), uuid.NewString(), "export")
			reader, writer := io.Pipe()
			defer reader.Close()
			done := make(chan error, 1)
			go func() {
				start := s.Begin(ExportProducer)
				if active {
					time.Sleep(30 * time.Millisecond)
				}
				err := binding.ExportOutputs(t.Context(), s.Writer(writer))
				s.End(ExportProducer, start, err)
				writer.CloseWithError(err)
				done <- err
			}()
			if !active {
				time.Sleep(30 * time.Millisecond)
			}
			got, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if err = binding.ExportOutputs(t.Context(), &want); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want.Bytes()) || len(got) != 1024 {
				t.Fatalf("writer changed empty tar: %d", len(got))
			}
			s.mu.Lock()
			producer, writes := s.metrics[ExportProducer], s.metrics[ExportWriter]
			s.mu.Unlock()
			if writes.count != 2 || writes.failed != 0 || writes.active != 0 {
				t.Fatal(writes)
			}
			if !active && writes.total < 20*time.Millisecond {
				t.Fatal("backpressure absent", writes)
			}
			if active && producer.total-writes.total < 20*time.Millisecond {
				t.Fatal("active work mislabeled as writer")
			}
		})
	}
}

type exactWriter struct {
	n     int
	err   error
	calls int
}

func (w *exactWriter) Write([]byte) (int, error) { w.calls++; return w.n, w.err }
func TestWriterPreservesResultAndUnknownSnapshot(t *testing.T) {
	s := New(t.Context(), uuid.NewString(), "export")
	want := errors.New("SECRET-CANARY")
	out := &exactWriter{n: 2, err: want}
	n, err := s.Writer(out).Write([]byte("private-payload"))
	if n != 2 || err != want || out.calls != 1 {
		t.Fatal(n, err, out.calls)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan struct{})
	go func() { s.Writer(writer).Write([]byte("x")); close(done) }()
	for {
		s.mu.Lock()
		active := s.metrics[ExportWriter].active
		s.mu.Unlock()
		if active == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	s.Finish("cancelled")
	slog.SetDefault(old)
	reader.CloseWithError(context.Canceled)
	<-done
	var row map[string]any
	if json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &row) != nil {
		t.Fatal(buf.String())
	}
	if row["export_writer_unknown_count"] != float64(1) || row["snapshot_complete"] != false || bytes.Contains(buf.Bytes(), []byte("SECRET")) {
		t.Fatal(row)
	}
}
func TestRoundBoundAndSnapshot(t *testing.T) {
	now := time.Now()
	received := ReceivedFrameContext(t.Context(), "connection", now, now, now)
	s := New(received, uuid.NewString(), "export")
	for i := 0; i < 12; i++ {
		ctx := s.RoundContext(t.Context(), received, "next", int64(i))
		RoundFromContext(ctx).Written(time.Now(), nil)
	}
	s.mu.Lock()
	if len(s.rounds) != 8 || !s.roundsTruncated || s.roundMetric.count != 12 {
		t.Fatal(s.roundCount)
	}
	s.mu.Unlock()
	ctx := s.RoundContext(t.Context(), received, "next", 12)
	s.Finish("cancelled")
	RoundFromContext(ctx).Written(time.Now(), nil)
}
func TestC2RuntimeProducerFixtures(t *testing.T) {
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
			now := time.Now()
			ctx := ReceivedFrameContext(t.Context(), "29117c12-a502-4d69-9ad5-27848a6b92ea", now, now, now)
			s := New(ctx, "54dedddf-27c9-4a06-9b1f-244d0bc373e5", "export")
			s.Writer(io.Discard).Write([]byte("fixture"))
			r := RoundFromContext(s.RoundContext(ctx, ctx, "begin", 0))
			r.Written(time.Now(), nil)
			s.Finish("completed")
			if directory := os.Getenv("B279_C2_FIXTURE_DIR"); directory != "" {
				if err := os.WriteFile(filepath.Join(directory, "runtime-"+format+".log"), buf.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestObservationMetricBounds(t *testing.T) {
	s := New(t.Context(), uuid.NewString(), "export")
	s.add(ExportWriter, -time.Nanosecond, nil)
	s.add(ExportWriter, 31*time.Minute, nil)
	if s.metrics[ExportWriter].count != 0 || s.metrics[ExportWriter].unknown != 2 {
		t.Fatal(s.metrics[ExportWriter])
	}
	s.metrics[ExportWriter].count = 1000000
	s.add(ExportWriter, time.Millisecond, nil)
	if s.metrics[ExportWriter].count != 1000000 || !s.metrics[ExportWriter].overflow {
		t.Fatal(s.metrics[ExportWriter])
	}
	s.roundCount = 1000000
	ctx := s.RoundContext(t.Context(), t.Context(), "next", 0)
	if RoundFromContext(ctx) != nil || !s.roundMetric.overflow {
		t.Fatal("round count did not saturate")
	}
}

func TestC2SaturationProducerFixtures(t *testing.T) {
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
			now := time.Now()
			ctx := ReceivedFrameContext(t.Context(), "29117c12-a502-4d69-9ad5-27848a6b92ea", now, now, now)
			s := New(ctx, "54dedddf-27c9-4a06-9b1f-244d0bc373e5", "export")
			s.metrics[ReadPredecode].unknown = 1000000
			s.Receive(ReceivedContext(t.Context(), s.connection, now))
			s.metrics[ExportWriter].unknown = 1000000
			s.metrics[ExportWriter].active = 1
			s.roundMetric.unknown = 1000000
			s.roundMetric.active = 1
			s.Finish("cancelled")
			if format == "json" {
				var row map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &row); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"read_return_to_decode_begin", "export_writer", "request_read_to_response_write"} {
					if row[name+"_unknown_count"] != float64(1000000) {
						t.Fatal(row)
					}
				}
				if row["rounds_overflow"] != true || row["read_return_to_decode_begin_overflow"] != true || row["export_writer_overflow"] != true || row["snapshot_complete"] != false {
					t.Fatal(row)
				}
			}
			if directory := os.Getenv("B279_C2_FIXTURE_DIR"); directory != "" {
				if err := os.WriteFile(filepath.Join(directory, "runtime-saturation-"+format+".log"), buf.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestWriteReturnBoundaryExcludesObserverLock(t *testing.T) {
	now := time.Now()
	ctx := ReceivedFrameContext(t.Context(), "connection", now, now, now)
	scope := New(ctx, "54dedddf-27c9-4a06-9b1f-244d0bc373e5", "export")
	round := RoundFromContext(scope.RoundContext(ctx, ctx, "begin", 0))
	started := scope.Begin(OutboundWrite)
	writeReturned := time.Now()
	scope.mu.Lock()
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(entered)
		scope.End(OutboundWrite, started, nil)
		round.Written(writeReturned, nil)
		close(done)
	}()
	<-entered
	time.Sleep(40 * time.Millisecond)
	scope.mu.Unlock()
	<-done
	actual := round.Duration.(float64)
	want := float64(writeReturned.Sub(now).Microseconds()) / 1000
	if actual != want {
		t.Fatalf("write-return boundary includes observer work: got %.6f want %.6f", actual, want)
	}
	if time.Since(writeReturned) < 40*time.Millisecond {
		t.Fatal("test did not inject observer contention")
	}
}

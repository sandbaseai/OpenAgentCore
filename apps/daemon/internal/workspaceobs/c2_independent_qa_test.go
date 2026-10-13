package workspaceobs

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Independent QA: cumulative unknown counters must retain the documented bound,
// including a missing arrival and the active operations included at snapshot.
func TestIndependentC2UnknownSaturation(t *testing.T) {
	s := New(context.Background(), "54dedddf-27c9-4a06-9b1f-244d0bc373e5", "export")
	s.metrics[ReadPredecode].unknown = 1000000
	now := time.Now()
	s.connection = "connection"
	s.Receive(ReceivedContext(context.Background(), "connection", now))
	if s.metrics[ReadPredecode].unknown > 1000000 || !s.metrics[ReadPredecode].overflow {
		t.Fatalf("missing-arrival count is not bounded: %+v", s.metrics[ReadPredecode])
	}
}
func TestIndependentC2InFlightRoundSaturation(t *testing.T) {
	s := New(context.Background(), "54dedddf-27c9-4a06-9b1f-244d0bc373e5", "export")
	s.roundMetric.unknown = 1000000
	s.roundMetric.active = 1
	fields := s.roundFieldsLocked()
	row := map[string]any{}
	for i := 0; i < len(fields); i += 2 {
		row[fields[i].(string)] = fields[i+1]
	}
	if row["request_read_to_response_write_unknown_count"].(int) > 1000000 || row["rounds_overflow"] != true {
		t.Fatalf("snapshot count exceeds collector contract: %+v", row)
	}
}
func TestIndependentC2RoundUnknownNullAndDoneOnce(t *testing.T) {
	s := New(context.Background(), "54dedddf-27c9-4a06-9b1f-244d0bc373e5", "export")
	r := RoundFromContext(s.RoundContext(context.Background(), context.Background(), "begin", 0))
	r.SetResponse("CANARY")
	r.Written(time.Now(), nil)
	r.Written(time.Now(), nil)
	if s.roundMetric.active != 0 || s.roundMetric.unknown != 1 || r.Observed || r.Duration != nil {
		t.Fatalf("unpaired completion %+v", r)
	}
	raw, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	var row map[string]any
	json.Unmarshal(raw, &row)
	if row["outcome"] != "unknown" || row["request_read_to_response_write_ms"] != nil {
		t.Fatal(string(raw))
	}
}

func TestIndependentC2ZeroAndOutOfRangeAreDistinct(t *testing.T) {
	s := New(context.Background(), "54dedddf-27c9-4a06-9b1f-244d0bc373e5", "export")
	s.add(ExportWriter, 0, nil)
	if s.metrics[ExportWriter].count != 1 || s.metrics[ExportWriter].total != 0 || s.metrics[ExportWriter].unknown != 0 {
		t.Fatal(s.metrics[ExportWriter])
	}
	s.add(ExportWriter, 30*time.Minute+time.Nanosecond, nil)
	if s.metrics[ExportWriter].count != 1 || s.metrics[ExportWriter].unknown != 1 || !s.metrics[ExportWriter].overflow {
		t.Fatal(s.metrics[ExportWriter])
	}
}

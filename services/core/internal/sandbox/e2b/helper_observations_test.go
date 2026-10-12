package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox"
)

func TestHelperStageFilterRejectsUnknownFieldsAndStages(t *testing.T) {
	valid := `{"event":"e2b_helper_stage","stage":"helper_startup","duration_us":1250,"completed":true}`
	for _, bad := range []string{strings.Replace(valid, "helper_startup", "SECRET", 1), strings.Replace(valid, "1250", "-1", 1), strings.Replace(valid, "1250", "1800000001", 1), strings.Replace(valid, "1250", "1.2", 1), strings.Replace(valid, "1250", "null", 1), strings.Replace(valid, "true", "null", 1), strings.TrimSuffix(valid, "}") + `,"credential":"SECRET"}`, valid + `{}`} {
		if got := parseStageObservations([]byte(bad), "e2b_helper_stage"); len(got) != 0 {
			t.Fatal("unsafe diagnostic accepted")
		}
	}
	if got := parseStageObservations([]byte(valid+"\n"+valid), "e2b_helper_stage"); len(got) != 1 {
		t.Fatal("duplicate stage escaped bound")
	}
}

func TestHelperCallAndStagesShareParentTraceWithoutExtendingWire(t *testing.T) {
	for _, code := range []string{"", "unconfirmed"} {
		t.Run(code, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
			defer slog.SetDefault(previous)
			root := t.TempDir()
			binary := filepath.Join(root, "helper")
			response := `{"Version":` + strconv.Itoa(ProtocolVersion) + `,"ErrorCode":"` + code + `"}`
			script := "#!/bin/sh\nprintf '%s\\n' '{\"event\":\"e2b_helper_stage\",\"stage\":\"helper_startup\",\"duration_us\":500,\"completed\":true}' '{\"event\":\"e2b_helper_stage\",\"stage\":\"helper_execute\",\"duration_us\":1500,\"completed\":false}' 'SECRET stderr' >&2\nprintf '%s' '" + response + "'\n"
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, carrier := obslog.StartBackgroundTrace(context.Background())
			q := Request{Version: ProtocolVersion, Operation: "list_templates", Config: Config{Binary: binary}, Deadline: time.Now().Add(time.Minute)}
			got, err := (&ProcessCaller{}).Call(ctx, q)
			if err != nil || got.ErrorCode != code {
				t.Fatal("wire result changed", got, err)
			}
			lines := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
			if len(lines) != 3 {
				t.Fatal("unexpected diagnostic count", logs.String())
			}
			for _, line := range lines {
				var row map[string]any
				if err := json.Unmarshal(line, &row); err != nil {
					t.Fatal(err)
				}
				if row["trace_id"] != carrier.Trace.String() || row["span_id"] != carrier.Span.String() || row["operation"] != "list_templates" || row["duration_ms"].(float64) < 0 {
					t.Fatal("lost correlation", row)
				}
				if row["msg"] == "e2b helper call" {
					want := "ok"
					if code != "" {
						want = "error"
					}
					if row["status"] != want || row["helper_started"] != true {
						t.Fatal(row)
					}
				}
			}
			if strings.Contains(logs.String(), "SECRET") {
				t.Fatal("raw stderr leaked")
			}
		})
	}
}

func TestHelperCallCancellationDoesNotAnnounceChildCompletion(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
	defer slog.SetDefault(previous)
	root := t.TempDir()
	binary := filepath.Join(root, "helper")
	marker := filepath.Join(root, "started")
	release := filepath.Join(root, "release")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"event\":\"e2b_helper_stage\",\"stage\":\"helper_startup\",\"duration_us\":1,\"completed\":true}' >&2\ntouch '" + marker + "'\nwhile ! test -f '" + release + "'; do sleep 0.01; done\nprintf '%s' '{\"Version\":" + strconv.Itoa(ProtocolVersion) + "}'\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fence := &sandbox.CallFence{}
	done := make(chan error, 1)
	go func() {
		_, err := (&ProcessCaller{Fence: fence}).Call(ctx, Request{Version: ProtocolVersion, Operation: "list_templates", Config: Config{Binary: binary}, Deadline: time.Now().Add(time.Minute)})
		done <- err
	}()
	defer func() {
		_ = os.WriteFile(release, nil, 0600)
		finish, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		unlock, err := fence.Fence(finish)
		if err != nil {
			t.Error("helper failed to drain", err)
		} else {
			unlock()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not reach controlled wait")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatal("caller cancellation changed", err)
	}
	var row map[string]any
	if err := json.Unmarshal(logs.Bytes(), &row); err != nil {
		t.Fatal("partial child diagnostics escaped before exit", err)
	}
	if row["msg"] != "e2b helper call" || row["status"] != "cancelled" || row["helper_started"] != true {
		t.Fatal(row)
	}
	for _, field := range []string{"process_wait_ms", "fence_release_ms", "observation_parse_ms", "response_decode_ms"} {
		if value, exists := row[field]; !exists || value != nil {
			t.Fatalf("cancelled caller invented %s", field)
		}
	}
	for _, field := range []string{"request_marshal_ms", "command_setup_ms", "process_start_ms", "caller_wait_ms"} {
		if value, ok := row[field].(float64); !ok || value < 0 {
			t.Fatalf("missing observed %s", field)
		}
	}
}

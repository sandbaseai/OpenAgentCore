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

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox"
)

func TestHelperProcessBoundariesPreserveOutcomes(t *testing.T) {
	for _, mode := range []string{"success", "fenced", "marshal_failure", "start_failure", "precancelled", "wait_failure", "decode_failure", "trailing", "stderr_overflow", "invalid_request"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			root := t.TempDir()
			binary := filepath.Join(root, "SECRET-path-helper")
			output := `{"Version":` + strconv.Itoa(ProtocolVersion) + `}`
			ending := ""
			if mode == "decode_failure" {
				output = `{"Version":1,"SECRET":"private"}`
			}
			if mode == "trailing" {
				output += ` {}`
			}
			if mode == "wait_failure" {
				ending = "exit 7\n"
			}
			script := "#!/bin/sh\nsleep 0.02\nprintf '%s' '" + output + "'\nprintf '%s\\n' 'SECRET-error-canary' >&2\n" + ending
			if mode == "stderr_overflow" {
				script = "#!/bin/sh\nhead -c " + strconv.Itoa(MaxOutputBytes+1) + " /dev/zero >&2\nprintf '%s' '" + output + "'\n"
			}
			if mode != "start_failure" {
				if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "precancelled" {
				cancel()
			}
			q := Request{Version: ProtocolVersion, Operation: "list_templates", Config: Config{Binary: binary}, Deadline: time.Now().Add(time.Minute)}
			if mode == "invalid_request" {
				q.Version = -1
			}
			if mode == "marshal_failure" {
				q.Deadline = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			caller := &ProcessCaller{}
			if mode == "fenced" {
				caller.Fence = &sandbox.CallFence{}
			}
			_, err := caller.Call(ctx, q)
			if (err == nil) != (mode == "success" || mode == "fenced") {
				t.Fatalf("changed outcome: %v", err)
			}
			if strings.Contains(logs.String(), "SECRET") {
				t.Fatal("private diagnostic leaked")
			}
			if mode == "invalid_request" {
				if logs.Len() != 0 {
					t.Fatal("validation added a call observation")
				}
				return
			}
			var row map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
				t.Fatal("not exactly one call summary", err)
			}
			require := func(field string, known bool) {
				t.Helper()
				v, ok := row[field]
				if !ok {
					t.Fatalf("missing %s", field)
				}
				if !known {
					if v != nil {
						t.Fatalf("invented %s", field)
					}
					return
				}
				if n, ok := v.(float64); !ok || n < 0 {
					t.Fatalf("bad %s", field)
				}
			}
			require("request_marshal_ms", true)
			require("command_setup_ms", mode != "marshal_failure")
			started := mode != "start_failure" && mode != "precancelled" && mode != "marshal_failure"
			require("process_start_ms", mode != "precancelled" && mode != "marshal_failure")
			require("caller_wait_ms", started)
			require("process_wait_ms", started)
			require("fence_release_ms", mode == "fenced")
			require("observation_parse_ms", started && mode != "stderr_overflow")
			require("response_decode_ms", started && mode != "wait_failure" && mode != "stderr_overflow")
			if row["helper_started"] != started {
				t.Fatal("started mismatch")
			}
			if started && mode != "stderr_overflow" && row["process_wait_ms"].(float64) < 15 {
				t.Fatal("Wait omitted controlled child delay")
			}
			if caller.Fence != nil {
				unlock, err := caller.Fence.Fence(ctx)
				if err != nil {
					t.Fatal(err)
				}
				unlock()
			}
		})
	}
}

func TestHelperOutputStagesStayInExistingFiniteFilter(t *testing.T) {
	var input strings.Builder
	for _, stage := range []string{"helper_startup", "helper_execute", "helper_response_serialize", "helper_stdout_write"} {
		for range 3 {
			input.WriteString(`{"event":"e2b_helper_stage","stage":"` + stage + `","duration_us":1,"completed":true}` + "\n")
		}
		input.WriteString(`{"event":"e2b_helper_stage","stage":"` + stage + `","duration_us":1,"completed":true,"SECRET":"private"}` + "\n")
	}
	input.WriteString(`{"event":"e2b_helper_stage","stage":"SECRET","duration_us":1,"completed":true}`)
	if got := parseStageObservations([]byte(input.String()), "e2b_helper_stage"); len(got) != 4 {
		t.Fatalf("finite stage bound %d", len(got))
	}
}

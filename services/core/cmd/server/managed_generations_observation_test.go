package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/deployment"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox/e2b"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox/providers"
	"github.com/google/uuid"
)

func TestCredentialVerificationFailureObservation(t *testing.T) {
	for _, tc := range []struct {
		fail             int
		phase            string
		generation, refs int
	}{
		{1, "committed_anchor", 3, 0}, {2, "candidate_current", 3, 0},
		{3, "candidate_selection", 3, 0}, {4, "retained_generation", 1, 0},
		{5, "allocation_references", 1, 1}, {0, "", 0, 0},
		{-1, "load_current", 0, 0}, {-2, "load_generations", 0, 0}, {-3, "load_allocations", 0, 0},
	} {
		t.Run(fmt.Sprint(tc.fail), func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			state := filepath.Join(t.TempDir(), "e2b")
			if err := os.MkdirAll(state, 0700); err != nil {
				t.Fatal(err)
			}
			helper := filepath.Join(t.TempDir(), "helper")
			script := fmt.Sprintf(`#!/usr/bin/env python3
import json,sys,pathlib
q=json.load(sys.stdin)
code=''
if q['Operation']=='verify_credential':
 p=pathlib.Path(q['Config']['StateDir'])/'count'
 count=int(p.read_text())+1 if p.exists() else 1
 p.write_text(str(count))
 if count==%d: code='team_mismatch'
out={'Version':q['Version'],'ErrorCode':code,'DeploymentValid':not code}
if q['Operation']=='validate_deployment': out['TemplateBuild']={'Status':'ready','CPUs':2,'MemoryMiB':2048}
print(json.dumps(out))
`, tc.fail)
			if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			current := deployment.Setup{InstallationID: uuid.NewString(), Provider: "e2b", Mode: "direct", Generation: 3, UsesCredential: true,
				Specification: sandbox.DeploymentSpec{Resources: sandbox.Resources{CPUs: 2, MemoryMiB: 2048}},
				Configuration: &e2b.DeploymentConfiguration{CredentialSupplied: true, APIKey: "PRIVATE-KEY", Template: "PRIVATE-TEMPLATE:" + uuid.NewString()}}
			retained := current
			retained.Generation = 1
			ref := deployment.Allocation{ID: uuid.NewString(), TenantID: uuid.NewString(), EnvironmentID: uuid.NewString(), DeploymentGeneration: 1}
			privateErr := errors.New("PRIVATE-ERROR")
			setups := &fakeDeploymentSetups{t: t,
				setup: func(context.Context) (deployment.Setup, error) {
					if tc.fail == -1 {
						return deployment.Setup{}, privateErr
					}
					return current, nil
				},
				withCredential: credentialService(t),
				generationPage: func(context.Context, int64) ([]deployment.Setup, error) {
					if tc.fail == -2 {
						return nil, privateErr
					}
					return []deployment.Setup{retained}, nil
				},
			}
			allocations := &fakeGenerationAllocations{t: t, credentialAllocations: func(context.Context, string) ([]deployment.Allocation, error) {
				if tc.fail == -3 {
					return nil, privateErr
				}
				return []deployment.Allocation{ref}, nil
			}}
			s := &managedSetup{capacity: testSandboxCapacity(t), processPaths: testProviderPaths(t, helper, state), registry: providers.Builtin(), installationID: current.InstallationID, deployment: setups, allocations: allocations}
			candidate, err := s.prepare(t.Context(), current)
			if err != nil {
				t.Fatal(err)
			}
			logs.Reset()
			err = candidate.VerifyCredential(t.Context())
			if tc.fail == 0 && err != nil {
				t.Fatal(err)
			}
			if tc.fail < 0 && err != privateErr {
				t.Fatal("read error identity changed")
			}
			if tc.fail == 1 {
				var reset *deployment.ResetRequiredError
				if !errors.As(err, &reset) {
					t.Fatal("anchor mapping changed")
				}
			}
			if tc.fail > 1 && !errors.Is(err, sandbox.ErrCredentialOwnership) {
				t.Fatal("ownership mapping changed")
			}
			observations := 0
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				if line == "" {
					continue
				}
				var row map[string]any
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					t.Fatal(err)
				}
				if row["msg"] != "sandbox credential verification failed" {
					continue
				}
				observations++
				if row["phase"] != tc.phase || row["generation"] != float64(tc.generation) || row["refs_count"] != float64(tc.refs) {
					t.Fatalf("wrong failure location: %v", row)
				}
				wantClass := "credential_ownership"
				if tc.fail == 1 {
					wantClass = "reset_required"
				}
				if tc.fail < 0 {
					wantClass = "internal_error"
				}
				if row["error_class"] != wantClass {
					t.Fatalf("wrong error class: %v", row)
				}
				for key := range row {
					switch key {
					case "time", "level", "msg", "phase", "generation", "refs_count", "error_class":
					default:
						t.Fatalf("unexpected diagnostic field %q", key)
					}
				}
			}
			want := 1
			if tc.fail == 0 {
				want = 0
			}
			if observations != want {
				t.Fatalf("got %d observations, want %d", observations, want)
			}
			for _, private := range []string{"PRIVATE-", current.InstallationID, ref.ID, ref.TenantID, ref.EnvironmentID} {
				if strings.Contains(logs.String(), private) {
					t.Fatal("private diagnostic value leaked")
				}
			}
		})
	}
}

func TestCredentialVerificationErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "cancelled"}, {context.DeadlineExceeded, "timeout"},
		{&deployment.ResetRequiredError{CurrentProvider: "PRIVATE", RequestedProvider: "PRIVATE"}, "reset_required"},
		{sandbox.ErrCredentialRejected, "credential_rejected"}, {sandbox.ErrCredentialOwnership, "credential_ownership"},
		{sandbox.ErrConfigurationUnconfirmed, "verification_unconfirmed"}, {errors.New("PRIVATE"), "internal_error"},
	} {
		if got := credentialVerificationErrorClass(fmt.Errorf("PRIVATE: %w", tc.err)); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}

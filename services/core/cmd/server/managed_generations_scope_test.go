package main

import (
	"context"
	"encoding/json"
	"errors"
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

func TestTemplateChangePreservesOwnershipWithoutRetainedResourceScan(t *testing.T) {
	for _, tc := range []struct {
		name                                                                                          string
		explicit, newKey, newURL, newDomain, resources, anchorFailure, candidateFailure, invalidBuild bool
		full                                                                                          bool
	}{
		{name: "template only"},
		{name: "resources only", resources: true},
		{name: "explicit same key", explicit: true, full: true},
		{name: "replacement key", explicit: true, newKey: true, full: true},
		{name: "API URL change", newURL: true, full: true},
		{name: "domain change", newDomain: true, full: true},
		{name: "invalid committed ownership", anchorFailure: true},
		{name: "invalid candidate ownership", candidateFailure: true},
		{name: "invalid candidate build", invalidBuild: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "e2b")
			if err := os.MkdirAll(state, 0700); err != nil {
				t.Fatal(err)
			}
			helper := filepath.Join(t.TempDir(), "helper")
			script := `#!/usr/bin/env python3
import json,sys,pathlib
q=json.load(sys.stdin)
with (pathlib.Path(q['Config']['StateDir'])/'requests').open('a') as f: f.write(json.dumps(q)+'\n')
code=''
if q['Operation']=='verify_credential':
 if q.get('References') or q['Config']['Template'].startswith('denied-'): code='team_mismatch'
out={'Version':q['Version'],'ErrorCode':code,'DeploymentValid':not code}
if q['Operation']=='validate_deployment':
 if q['Config']['Template'].startswith('invalid-'): out['ErrorCode']='invalid'
 else: out['TemplateBuild']={'Status':'ready','CPUs':q['Config']['Resources']['cpus'],'MemoryMiB':2048}
print(json.dumps(out))
`
			if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			build := ":" + uuid.NewString()
			currentConfig := &e2b.DeploymentConfiguration{APIKey: "fixture-key", Template: "old" + build, APIURL: "https://api.e2b.app", Domain: "e2b.app"}
			if tc.anchorFailure {
				currentConfig.Template = "denied-old" + build
			}
			current := deployment.Setup{InstallationID: uuid.NewString(), Provider: "e2b", Mode: "direct", Generation: 13, UsesCredential: true, Configuration: currentConfig, Specification: sandbox.DeploymentSpec{Resources: sandbox.Resources{CPUs: 2, MemoryMiB: 2048}}}
			// Exercise request omission/inheritance, rather than manufacturing its flag.
			request := &e2b.DeploymentConfiguration{Template: "new" + build}
			if tc.resources {
				request.Template = currentConfig.Template
			}
			if tc.explicit {
				request.APIKey = currentConfig.APIKey
				request.CredentialSupplied = true
			}
			if tc.newKey {
				request.APIKey = "replacement-fixture"
			}
			if tc.newURL {
				request.APIURL = "https://other.e2b.app"
				request.Domain = "e2b.app"
			}
			if tc.newDomain {
				request.APIURL = currentConfig.APIURL
				request.Domain = "api.e2b.app"
			}
			if tc.candidateFailure {
				request.Template = "denied-new" + build
			}
			if tc.invalidBuild {
				request.Template = "invalid-new" + build
			}
			resources := current.Specification.Resources
			if tc.resources {
				resources.CPUs = 4
			}
			resolved, err := providers.Builtin().ResolveChange(sandbox.Selection{Provider: "e2b", Configuration: request, DeploymentSpec: sandbox.DeploymentSpec{Resources: resources}}, sandbox.Selection{Provider: "e2b", Configuration: currentConfig, DeploymentSpec: current.Specification})
			if err != nil {
				t.Fatal(err)
			}
			next := current
			next.Configuration = resolved.Configuration
			next.Specification = resolved.DeploymentSpec
			scans := 0
			setups := &fakeDeploymentSetups{t: t, setup: func(context.Context) (deployment.Setup, error) { return current, nil }, withCredential: credentialService(t), generationPage: func(context.Context, int64) ([]deployment.Setup, error) {
				scans++
				return []deployment.Setup{current}, nil
			}}
			allocations := &fakeGenerationAllocations{t: t, credentialAllocations: func(context.Context, string) ([]deployment.Allocation, error) {
				scans++
				return []deployment.Allocation{{ID: uuid.NewString(), TenantID: uuid.NewString(), EnvironmentID: uuid.NewString(), DeploymentGeneration: 13}}, nil
			}}
			s := &managedSetup{capacity: testSandboxCapacity(t), processPaths: testProviderPaths(t, helper, state), registry: providers.Builtin(), installationID: current.InstallationID, deployment: setups, allocations: allocations}
			candidate, err := s.prepare(t.Context(), next)
			if tc.invalidBuild {
				if !errors.Is(err, sandbox.ErrConfigurationSelection) || scans != 0 {
					t.Fatalf("invalid build admitted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = candidate.VerifyCredential(t.Context())
			if tc.anchorFailure {
				var reset *deployment.ResetRequiredError
				if !errors.As(err, &reset) {
					t.Fatalf("missing ownership anchor accepted: %v", err)
				}
			} else if tc.full || tc.candidateFailure {
				if !errors.Is(err, sandbox.ErrCredentialOwnership) {
					t.Fatalf("ownership failure lost: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if (scans > 0) != tc.full {
				t.Fatalf("retained scan=%d, want full=%v", scans, tc.full)
			}
			raw, err := os.ReadFile(filepath.Join(state, "requests"))
			if err != nil {
				t.Fatal(err)
			}
			var verification []e2b.Request
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var q e2b.Request
				if err := json.Unmarshal([]byte(line), &q); err != nil {
					t.Fatal(err)
				}
				if q.Operation == "verify_credential" {
					verification = append(verification, q)
				} else if q.Operation != "validate_deployment" {
					t.Fatal("unexpected provider mutation")
				}
			}
			if len(verification) == 0 || verification[0].Config.Template != currentConfig.Template {
				t.Fatal("committed anchor was not verified first")
			}
			if !tc.full && !tc.anchorFailure {
				if len(verification) != 2 || verification[1].Config.Template != request.Template {
					t.Fatal("candidate ownership not independently verified")
				}
			}
			if current.Generation != 13 || currentConfig.APIKey != "fixture-key" {
				t.Fatal("verification changed committed state")
			}
		})
	}
}

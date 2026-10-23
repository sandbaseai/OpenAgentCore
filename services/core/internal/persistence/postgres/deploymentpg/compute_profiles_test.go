package deploymentpg_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/deployment"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/deployment/placement"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/identity"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgunit"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/sessionpg"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox/e2b"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox/providers"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestComputeProfileFrozenBeforeAllocationSurvivesReconfiguration(t *testing.T) {
	f := newFixture(t)
	changes, _ := f.execution(t)
	input := setupE2BSelection()
	input.ComputeProfiles = map[string]sandbox.ComputeProfile{"memory-8g": {Resources: sandbox.Resources{CPUs: 4, MemoryMiB: 8192}, Configuration: &e2b.DeploymentConfiguration{Template: "large:" + uuid.NewString()}}}
	installation, view := f.initialize(t, changes, input)
	if view.Generation != 2 || view.Specification.Resources.MemoryMiB != 2048 || view.ComputeProfiles["memory-8g"].Generation != 1 {
		t.Fatal("default or profile generation incorrect")
	}
	rules, err := placement.NewRules(providers.Builtin(), fixturePublicURL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := sessions.NewService(sessionpg.New(pgunit.NewPool(f.pool), f.cipher), rules)
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.NewString()
	create := sessions.CreateSession{Creator: identity.Subject{Kind: "service_account", ID: "compute-profile-test"}, Engine: "codex", IdempotencyKey: "profile", Configuration: json.RawMessage(`{"environment":{"type":"openai_hosted"},"compute_profile":"memory-8g"}`)}
	type outcome struct {
		result sessions.Creation
		err    error
	}
	outcomes := make(chan outcome, 8)
	for range 8 {
		go func() {
			result, err := service.CreateSession(t.Context(), tenant, create)
			outcomes <- outcome{result, err}
		}()
	}
	var first sessions.Creation
	fresh := 0
	for range 8 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if first.Session.ID != "" && first.Session.ID != outcome.result.Session.ID {
			t.Fatal("concurrent retry created another session")
		}
		if outcome.result.Created {
			fresh++
		}
		first = outcome.result
	}
	if fresh != 1 {
		t.Fatal("concurrent retry did not elect one creator", fresh)
	}
	if first.Session.Environment == nil {
		t.Fatal("no environment")
	}
	// Remove the named choice before provisioning; a new session must fail,
	// while the original retry and future allocation retain the immutable choice.
	input.ComputeProfiles = nil
	input.ExpectedGeneration = view.Generation
	next, err := changes.Update(admin(t), installation, input)
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != 3 || next.Specification.Resources.MemoryMiB != 2048 {
		t.Fatal("default unexpectedly changed")
	}
	if err = changes.CollectGenerations(t.Context()); err != nil {
		t.Fatal(err)
	}
	replay, err := service.CreateSession(t.Context(), tenant, create)
	if err != nil || replay.Created || replay.Session.ID != first.Session.ID {
		t.Fatal("retry lost frozen profile", err)
	}
	create.IdempotencyKey = "missing"
	if _, err = service.CreateSession(t.Context(), tenant, create); !errors.Is(err, placement.ErrComputeProfileUnavailable) {
		t.Fatal("unknown profile did not fail closed", err)
	}
	key := deployment.AllocationKey{TenantID: tenant, EnvironmentID: first.Session.Environment.ID}
	allocation, err := changes.ReserveAllocation(t.Context(), key, installation, credentialHash())
	if err != nil {
		t.Fatal(err)
	}
	if allocation.DeploymentGeneration != 1 {
		t.Fatal("frozen allocation changed generation")
	}
	setup, err := f.service.AllocationSetup(t.Context(), sandbox.Reference{TenantID: tenant, EnvironmentID: key.EnvironmentID, AllocationID: allocation.ID})
	if err != nil {
		t.Fatal(err)
	}
	if setup.Specification.Resources.MemoryMiB != 8192 || setup.Specification.Resources.CPUs != 4 {
		t.Fatal("wrong retained resources")
	}
	ordinary := hostedEnvironment(t, f.pool)
	other, err := changes.ReserveAllocation(t.Context(), ordinary, installation, credentialHash())
	if err != nil || other.DeploymentGeneration != 3 {
		t.Fatal("ordinary allocation did not keep default", err)
	}
}

func TestComputeProfileDeploymentSnapshotKeepsExecutionDeadline(t *testing.T) {
	f := newFixture(t)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), "LOCK TABLE runtime_deployment IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = f.adapter.Deployment(context.Background())
	if err == nil || time.Since(started) > pgunit.ExecutionTimeout+2*time.Second {
		t.Fatal("deployment snapshot lost its execution deadline", err)
	}
}

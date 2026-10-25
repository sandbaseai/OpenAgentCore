package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/deployment"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/workspacefs"
	"testing"
)

func TestReleasedDirectAllocationStillChecksCapacity(t *testing.T) {
	for _, limit := range []string{"active", "retained"} {
		t.Run(limit, func(t *testing.T) {
			r := runtimeLifecycle{sessions: workspaceEnvironmentReader{}, config: RuntimeProvider{Mode: "direct", InstallationID: "fixture", Generation: 1, Suspension: &RuntimeSuspensionPolicy{MaxActive: 1, MaxRetained: 2}}, reader: &strictDeploymentReader{t: t,
				environmentAllocation: func(context.Context, deployment.AllocationKey) (deployment.Allocation, error) {
					return deployment.Allocation{State: "released"}, nil
				},
				countComputeReservations: func(context.Context, string) (int64, error) {
					if limit == "active" {
						return 1, nil
					}
					return 0, nil
				},
				countRetainedAllocations: func(context.Context, string) (int64, error) { return 2, nil },
			}}
			if _, err := r.provision(t.Context(), "tenant", "environment", "fixture"); !errors.Is(err, ErrExecutionUnavailable) {
				t.Fatal("released owner bypassed direct capacity", err)
			}
		})
	}
}

func TestRestoreUsesAllocationGenerationWorkspaceAfterDeploymentChange(t *testing.T) {
	owner := deployment.Allocation{ID: "allocation", ProviderKey: "installation", DeploymentGeneration: 4}
	for _, originalExternal := range []bool{false, true} {
		t.Run(fmt.Sprint(originalExternal), func(t *testing.T) {
			original, current := sandbox.DeploymentSpec{}, sandbox.DeploymentSpec{Workspace: &workspacefs.Declaration{}}
			if originalExternal {
				original, current = current, original
			}
			prior, _ := json.Marshal(original)
			r := runtimeLifecycle{config: RuntimeProvider{Workspace: current.Workspace}, reader: &strictDeploymentReader{t: t, lifecyclePlacement: func(context.Context, deployment.AllocationKey) (deployment.LifecyclePlacement, error) {
				return deployment.LifecyclePlacement{AllocationID: owner.ID, Specification: prior}, nil
			}}}

			binding, err := r.restoreWorkspace(t.Context(), owner)
			if originalExternal {
				if !errors.Is(err, workspacefs.ErrUnavailable) {
					t.Fatal("original external storage was bypassed", err)
				}
			} else if err != nil || binding != nil {
				t.Fatal("old owned disk depended on new workspace", err)
			}
		})
	}
}

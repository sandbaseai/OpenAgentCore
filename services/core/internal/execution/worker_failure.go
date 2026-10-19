package execution

import (
	"context"
	"fmt"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
)

func observeWorkerFailure(ctx context.Context, stage string, err error) {
	obslog.Ctx(ctx).Error("execution worker stopped", "stage", stage, "error_type", fmt.Sprintf("%T", err))
}

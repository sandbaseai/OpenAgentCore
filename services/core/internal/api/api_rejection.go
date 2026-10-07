package api

import (
	"context"
	"net/http"

	"github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
)

// Reasons are assigned only at explicit rejection branches, never from error
// messages or request data. The zero value leaves an unclassified failure unknown.
type apiRejectionReason uint8

const (
	rejectionUnknown apiRejectionReason = iota
	rejectionInvalidPayload
	rejectionUnknownField
	rejectionInvalidPath
	rejectionInvalidBase64
	rejectionInlineTooLarge
	rejectionHostedEnvironmentProvisioning
	rejectionDestinationDirectory
	rejectionDestinationUnsafe
)

func (reason apiRejectionReason) String() string {
	switch reason {
	case rejectionInvalidPayload:
		return "invalid_payload"
	case rejectionUnknownField:
		return "unknown_field"
	case rejectionInvalidPath:
		return "invalid_path"
	case rejectionInvalidBase64:
		return "invalid_base64"
	case rejectionInlineTooLarge:
		return "inline_too_large"
	case rejectionHostedEnvironmentProvisioning:
		return "hosted_environment_provisioning"
	case rejectionDestinationDirectory:
		return "destination_directory"
	case rejectionDestinationUnsafe:
		return "destination_unsafe"
	default:
		return "unknown"
	}
}

// The existing response writer owns diagnostics as well as response timing.
// Unwrapping preserves the controller's deadline/flush path and adds no wrapper.
// A non-nil context enables the diagnostic using the handler context after
// tracing middleware has run. Later rejection branches pass nil to only mark a reason.
func environmentFileDiagnostic(w http.ResponseWriter, ctx context.Context, reason apiRejectionReason) {
	for w != nil {
		if writer, ok := w.(*processingTimeWriter); ok {
			if !writer.stamped && (ctx != nil || writer.environmentFile) {
				if ctx != nil {
					writer.ctx = ctx
				}
				writer.environmentFile = true
				writer.rejectionReason = reason
			}
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}

func safeAPIErrorCode(code string) string {
	switch code {
	case "invalid_request", "invalid_request_error", "not_found_error", "request_too_large", "conflict_error", "server_error", "execution_unavailable":
		return code
	default:
		return "unknown"
	}
}

func (w *processingTimeWriter) logRejection(status int) {
	if !w.environmentFile || status < 400 || w.stamped {
		return
	}
	log.Warn(w.ctx, "api_request_rejected",
		"operation", "environment_file_create",
		"route", "/v1/agents/environments/{environment_id}/files",
		"status", status,
		"code", safeAPIErrorCode(w.errorCode),
		"reason", w.rejectionReason.String())
}

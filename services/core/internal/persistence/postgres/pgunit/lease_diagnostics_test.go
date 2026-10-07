package pgunit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestLeaseDiagnosticsRetainClassWithoutConfidentialErrorText(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	err := &pgconn.PgError{Code: "57014", Message: "secret-canary", Detail: "secret-canary", Where: "secret-canary", InternalQuery: "secret-canary"}
	observeLeaseFailure(t.Context(), t.Context(), "transaction", "connection", time.Now(), err, nil)
	if strings.Contains(output.String(), "secret-canary") || !strings.Contains(output.String(), `"sqlstate":"57014"`) || !strings.Contains(output.String(), `"connection_closed":true`) {
		t.Fatal("unsafe or incomplete lease failure diagnostic", output.String())
	}
	for _, tt := range []struct {
		err  error
		want string
	}{{nil, "none"}, {context.Canceled, "cancelled"}, {context.DeadlineExceeded, "deadline_exceeded"}, {pgconn.ErrConnClosed, "connection_closed"}, {ErrLeaseClosed, "lease_closed"}, {err, "postgres_error"}, {errors.New("secret-canary"), "operation_error"}} {
		if got := leaseErrorClass(tt.err); got != tt.want {
			t.Fatalf("class=%s want=%s", got, tt.want)
		}
	}
}

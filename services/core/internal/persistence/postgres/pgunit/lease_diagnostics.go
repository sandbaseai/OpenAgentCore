package pgunit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Never log error text: PostgreSQL errors can contain SQL and confidential data.
func leaseErrorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, pgconn.ErrConnClosed):
		return "connection_closed"
	case errors.Is(err, ErrLeaseClosed):
		return "lease_closed"
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) {
		return "postgres_error"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return "network_error"
	}
	return "operation_error"
}

func observeLeaseFailure(caller, operationContext context.Context, operation, phase string, started time.Time, err error, conn *pgxpool.Conn) {
	fields := []any{"operation", operation, "phase", phase, "duration_ms", float64(time.Since(started)) / float64(time.Millisecond), "error_class", leaseErrorClass(err), "error_type", fmt.Sprintf("%T", err), "caller_context", leaseErrorClass(caller.Err()), "operation_context", leaseErrorClass(operationContext.Err())}
	if phase == "connection" {
		fields = append(fields, "connection_closed", conn == nil || conn.Conn().PgConn().IsClosed())
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) {
		fields = append(fields, "sqlstate", pgError.Code)
	}
	obslog.Ctx(caller).Warn("execution lease operation failed", fields...)
}

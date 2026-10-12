package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox"
)

// ProcessCaller retains and drains an outstanding helper after caller timeout.
// The helper keeps its allocation lock until its bounded SDK operation settles.
var errHelperNotStarted = errors.New("helper did not start")

type ProcessCaller struct{ Fence *sandbox.CallFence }

func (p *ProcessCaller) Call(ctx context.Context, q Request) (response Response, callErr error) {
	if q.Validate() != nil {
		return Response{}, errHelperNotStarted
	}
	started := time.Now()
	helperStarted := false
	var requestMarshalMS, commandSetupMS, processStartMS, callerWaitMS any
	var processWaitMS, fenceReleaseMS, observationParseMS, responseDecodeMS any
	defer func() {
		status := "ok"
		switch {
		case errors.Is(callErr, context.Canceled):
			status = "cancelled"
		case errors.Is(callErr, context.DeadlineExceeded):
			status = "timeout"
		case callErr != nil || response.ErrorCode != "":
			status = "error"
		}
		obslog.Ctx(ctx).Info("e2b helper call", "operation", q.Operation, "duration_ms", float64(time.Since(started).Microseconds())/1000, "status", status, "helper_started", helperStarted, "environment_id", q.Reference.EnvironmentID, "allocation_id", q.Reference.AllocationID,
			"request_marshal_ms", requestMarshalMS, "command_setup_ms", commandSetupMS,
			"process_start_ms", processStartMS, "caller_wait_ms", callerWaitMS,
			"process_wait_ms", processWaitMS, "fence_release_ms", fenceReleaseMS,
			"observation_parse_ms", observationParseMS, "response_decode_ms", responseDecodeMS)
	}()
	boundary := time.Now()
	data, err := json.Marshal(q)
	requestMarshalMS = float64(time.Since(boundary).Microseconds()) / 1000
	if err != nil || len(data) > MaxRequestBytes {
		return Response{}, errHelperNotStarted
	}
	boundary = time.Now()
	cmd := exec.Command(q.Config.Binary)
	cmd.Stdin = bytes.NewReader(data)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "HOME" || key == "PATH" || key == "TMPDIR" || key == "LANG" || key == "SSL_CERT_FILE" || key == "SSL_CERT_DIR" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1")
	stdout, stderr := &limitBuffer{limit: MaxResponseBytes}, &limitBuffer{limit: MaxOutputBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	commandSetupMS = float64(time.Since(boundary).Microseconds()) / 1000
	if ctx.Err() != nil {
		return Response{}, errHelperNotStarted
	}
	boundary = time.Now()
	startErr := cmd.Start()
	processStartMS = float64(time.Since(boundary).Microseconds()) / 1000
	if startErr != nil {
		return Response{}, errHelperNotStarted
	}
	helperStarted = true
	finished := func() {}
	if p.Fence != nil {
		finished = p.Fence.ChildStarted()
	}
	// Only the existing completion channel transfers child measurements. A
	// cancelled caller never reads buffers or fields still owned by cmd.Wait.
	type completion struct {
		err     error
		waitMS  float64
		fenceMS any
	}
	done := make(chan completion, 1)
	hasFence := p.Fence != nil
	go func() {
		waitStarted := time.Now()
		err := cmd.Wait()
		result := completion{err: err, waitMS: float64(time.Since(waitStarted).Microseconds()) / 1000}
		releaseStarted := time.Now()
		finished()
		if hasFence {
			result.fenceMS = float64(time.Since(releaseStarted).Microseconds()) / 1000
		}
		done <- result
	}()
	boundary = time.Now()
	select {
	case <-ctx.Done():
		callerWaitMS = float64(time.Since(boundary).Microseconds()) / 1000
		return Response{}, ctx.Err()
	case result := <-done:
		callerWaitMS = float64(time.Since(boundary).Microseconds()) / 1000
		err, processWaitMS, fenceReleaseMS = result.err, result.waitMS, result.fenceMS
		if !stderr.exceeded {
			parseStarted := time.Now()
			observeHelperStages(ctx, q, stderr.Bytes())
			observationParseMS = float64(time.Since(parseStarted).Microseconds()) / 1000
		}
		if err != nil || stdout.exceeded || stderr.exceeded {
			return Response{}, errors.New("helper result unconfirmed")
		}
	}
	decodeStarted := time.Now()
	defer func() { responseDecodeMS = float64(time.Since(decodeStarted).Microseconds()) / 1000 }()
	var out Response
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&out) != nil || out.Validate() != nil {
		return Response{}, errors.New("invalid helper response")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Response{}, errors.New("trailing helper response")
	}
	return out, nil
}

type limitBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

// Do not let the embedded bytes.Buffer's ReaderFrom bypass Write's limit when
// os/exec copies a child's output with io.Copy.
func (b *limitBuffer) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{b}, r)
}

func (b *limitBuffer) Write(data []byte) (int, error) {
	n := len(data)
	left := b.limit - b.Len()
	if n > left {
		b.exceeded = true
		data = data[:left]
	}
	_, _ = b.Buffer.Write(data)
	return n, nil
}

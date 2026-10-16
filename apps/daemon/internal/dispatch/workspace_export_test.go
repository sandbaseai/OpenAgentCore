package dispatch

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/iotest"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/agent"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/localworkspace"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/google/uuid"
)

type exportSender struct{ replies chan proto.Envelope }

func (s exportSender) Send(ctx context.Context, env proto.Envelope) error {
	select {
	case s.replies <- env:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func exporterRouter(t *testing.T, program string) (*Router, exportSender, proto.WorkspaceExportPayload) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "outputs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "outputs", "a"), make([]byte, 131089), 0600); err != nil {
		t.Fatal(err)
	}
	if program == "failure" {
		f, err := os.Create(filepath.Join(workspace, "outputs", "z"))
		if err != nil {
			t.Fatal(err)
		}
		if err = f.Truncate(201 << 20); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	environment, session := uuid.NewString(), uuid.NewString()
	binding, err := localworkspace.NewWithCapabilityDirectory(environment, session, workspace, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sender := exportSender{make(chan proto.Envelope, 8)}
	r, err := New(Config{Registry: agent.NewRegistry(), Sender: sender, LocalWorkspace: binding})
	if err != nil {
		t.Fatal(err)
	}
	handle := uuid.NewString()
	r.preparations[handle] = &preparationState{environmentID: environment, owns: true, ctx: context.Background(), deadline: time.Now().Add(time.Hour), status: proto.PreparationStatusPayload{State: "ready"}}
	t.Cleanup(func() {
		r.mu.Lock()
		delete(r.preparations, handle)
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := r.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return r, sender, proto.WorkspaceExportPayload{Step: "begin", Handle: handle, EnvironmentID: environment}
}

func sendExport(t *testing.T, r *Router, id string, p proto.WorkspaceExportPayload) {
	t.Helper()
	env, err := proto.NewEnvelope(proto.TypeWorkspaceExport, id, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(t.Context(), env); err != nil {
		t.Fatal(err)
	}
}
func readExport(t *testing.T, s exportSender) proto.WorkspaceExportResultPayload {
	t.Helper()
	select {
	case env := <-s.replies:
		var p proto.WorkspaceExportResultPayload
		if env.DecodePayload(&p) != nil {
			t.Fatal("invalid reply")
		}
		return p
	case <-time.After(3 * time.Second):
		t.Fatal("missing export reply")
	}
	return proto.WorkspaceExportResultPayload{}
}

func TestWorkspaceExportUsesExactReadPreparationAndPullsBoundedBytes(t *testing.T) {
	r, s, request := exporterRouter(t, "normal")
	for _, field := range []string{"environment", "handle"} {
		bad := request
		if field == "environment" {
			bad.EnvironmentID = uuid.NewString()
		} else {
			bad.Handle = uuid.NewString()
		}
		sendExport(t, r, uuid.NewString(), bad)
		if result := readExport(t, s); result.Outcome != "rejected" {
			t.Fatal("foreign authority accepted")
		}
	}
	id := uuid.NewString()
	sendExport(t, r, id, request)
	var offset int64
	var archive bytes.Buffer
	chunks := 0
	for {
		p := readExport(t, s)
		if p.Offset != offset {
			t.Fatal("wrong offset")
		}
		if p.Outcome == "completed" {
			break
		}
		if p.Outcome != "chunk" || len(p.Data) == 0 || len(p.Data) > proto.WorkspaceExportChunkBytes {
			t.Fatal("invalid chunk")
		}
		archive.Write(p.Data)
		chunks++
		offset += int64(len(p.Data))
		select {
		case <-s.replies:
			t.Fatal("export pushed unrequested data")
		default:
		}
		sendExport(t, r, id, proto.WorkspaceExportPayload{Step: "next", Offset: offset})
	}
	if want := (archive.Len() + proto.WorkspaceExportChunkBytes - 1) / proto.WorkspaceExportChunkBytes; chunks != want {
		t.Fatalf("archive used %d chunks, want %d", chunks, want)
	}
	reader := tar.NewReader(&archive)
	header, err := reader.Next()
	if err != nil || header.Name != "outputs/a" || header.Size != 131089 {
		t.Fatal("invalid archive", header, err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(data, make([]byte, 131089)) {
		t.Fatal("truncated artifact", err)
	}
	if _, err = reader.Next(); err != io.EOF {
		t.Fatal("unexpected extra artifact", err)
	}
}

type exportErrorReader struct {
	data []byte
	err  error
}

func (r exportErrorReader) Read(p []byte) (int, error) { return copy(p, r.data), r.err }

func TestWorkspaceExportCoalescesShortReadsAndPreservesBytes(t *testing.T) {
	for _, size := range []int{0, 1, proto.WorkspaceExportChunkBytes, 2*proto.WorkspaceExportChunkBytes + 17, 8<<20 + 17} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := make([]byte, size)
			for i := range body {
				body[i] = byte(i * 37)
			}
			reader := iotest.HalfReader(bytes.NewReader(body))
			buffer := make([]byte, proto.WorkspaceExportChunkBytes)
			var got bytes.Buffer
			chunks := 0
			for {
				result := readWorkspaceExportChunk(reader, buffer, int64(got.Len()))
				if result.Outcome == "completed" {
					break
				}
				if result.Outcome != "chunk" || result.Offset != int64(got.Len()) {
					t.Fatal("invalid chunk", result.Outcome, result.Offset)
				}
				got.Write(result.Data)
				chunks++
			}
			if sha256.Sum256(got.Bytes()) != sha256.Sum256(body) || chunks != (size+len(buffer)-1)/len(buffer) {
				t.Fatalf("bytes or chunk count differ: %d bytes, %d chunks", got.Len(), chunks)
			}
		})
	}
}

func TestWorkspaceExportChunkRejectsPartialFailureAndOverflow(t *testing.T) {
	for _, err := range []error{errors.New("export failed"), io.ErrUnexpectedEOF, context.Canceled} {
		result := readWorkspaceExportChunk(exportErrorReader{[]byte("partial"), err}, make([]byte, proto.WorkspaceExportChunkBytes), 0)
		if result.Outcome != "failed" || len(result.Data) != 0 || result.ErrorCode != "export_failed" {
			t.Fatal("partial source failure escaped", result.Outcome, err)
		}
	}
	for _, offset := range []int64{proto.WorkspaceExportMaxBytes - 1, proto.WorkspaceExportMaxBytes} {
		result := readWorkspaceExportChunk(bytes.NewReader([]byte("xx")), make([]byte, proto.WorkspaceExportChunkBytes), offset)
		if result.Outcome != "failed" || len(result.Data) != 0 {
			t.Fatal("oversize source accepted")
		}
	}
	result := readWorkspaceExportChunk(bytes.NewReader(nil), make([]byte, proto.WorkspaceExportChunkBytes), proto.WorkspaceExportMaxBytes)
	if result.Outcome != "completed" {
		t.Fatal("exact bound completion rejected")
	}
}

func TestWorkspaceExportFailureAfterBytesCannotComplete(t *testing.T) {
	r, s, request := exporterRouter(t, "failure")
	id := uuid.NewString()
	sendExport(t, r, id, request)
	var offset int64
	for {
		p := readExport(t, s)
		if p.Outcome == "failed" {
			if offset == 0 {
				t.Fatal("no prefix before failure")
			}
			break
		}
		if p.Outcome != "chunk" || p.Offset != offset {
			t.Fatal("failed export appeared complete", p)
		}
		offset += int64(len(p.Data))
		sendExport(t, r, id, proto.WorkspaceExportPayload{Step: "next", Offset: offset})
	}
}

func TestWorkspaceExportCancelUnblocksWriterAndReleasesCapacity(t *testing.T) {
	r, _, request := exporterRouter(t, "normal")
	id := uuid.NewString()
	sendExport(t, r, id, request)
	sendExport(t, r, id, proto.WorkspaceExportPayload{Step: "cancel"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		active := r.workspaceExport != nil
		r.mu.Unlock()
		if !active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("export cancellation did not release capacity")
}

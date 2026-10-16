package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimegateway"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

// deliveryConn models a delayed runtime transport. Each receipt requires a pull;
// it does not autonomously push the archive or buffer a producer goroutine.
type deliveryConn struct {
	archive  []byte
	incoming chan []byte
	closed   chan struct{}
	pulls    int
}

func (c *deliveryConn) ReadMessage() (int, []byte, error) {
	select {
	case b := <-c.incoming:
		return 1, b, nil
	case <-c.closed:
		return 0, nil, io.EOF
	}
}
func (c *deliveryConn) WriteMessage(_ int, b []byte) error {
	var env proto.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return err
	}
	if env.Type != proto.TypeWorkspaceExport {
		return fmt.Errorf("unexpected request %s", env.Type)
	}
	var request proto.WorkspaceExportPayload
	if err := env.DecodePayload(&request); err != nil {
		return err
	}
	if request.Step == "cancel" {
		return nil
	}
	time.Sleep(time.Millisecond)
	c.pulls++
	offset := int(request.Offset)
	if offset < 0 || offset > len(c.archive) {
		return fmt.Errorf("bad offset")
	}
	result := proto.WorkspaceExportResultPayload{Outcome: "completed", Offset: request.Offset}
	if offset < len(c.archive) {
		end := min(offset+proto.WorkspaceExportChunkBytes, len(c.archive))
		result.Outcome = "chunk"
		result.Data = c.archive[offset:end]
	}
	reply, err := proto.NewEnvelope(proto.TypeWorkspaceExportResult, env.ID, result)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	select {
	case c.incoming <- raw:
		return nil
	case <-c.closed:
		return io.EOF
	}
}
func (*deliveryConn) WriteControl(int, []byte, time.Time) error { return nil }
func (*deliveryConn) SetReadLimit(int64)                        {}
func (*deliveryConn) SetReadDeadline(time.Time) error           { return nil }
func (*deliveryConn) SetWriteDeadline(time.Time) error          { return nil }
func (c *deliveryConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

// Only the staging storage boundary is substituted. Parsing, transport, staging
// orchestration and the public binary HTTP handler are production code.
type deliveryStorage struct {
	sessions.Storage
	data   map[string][]byte
	staged bool
}
type deliveryTx struct {
	data   map[string][]byte
	staged bool
}

func (s *deliveryStorage) WithArtifactStaging(ctx context.Context, _ sessions.ArtifactStagingKey, f func(context.Context, sessions.ArtifactStagingTx) error) error {
	tx := &deliveryTx{data: map[string][]byte{}}
	if err := f(ctx, tx); err != nil {
		return err
	}
	s.data, s.staged = tx.data, tx.staged
	return nil
}
func (*deliveryTx) LoadEnvironment(context.Context) (sessions.Environment, error) {
	return sessions.Environment{ID: "environment", Configuration: json.RawMessage(`{"type":"openai_hosted"}`)}, nil
}
func (tx *deliveryTx) PutArtifactContent(_ context.Context, path string, size int64, r io.Reader) error {
	b, err := io.ReadAll(io.LimitReader(r, size))
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return io.ErrUnexpectedEOF
	}
	tx.data[path] = b
	return nil
}
func (*deliveryTx) LockSession(context.Context) (sessions.LockedSession, error) {
	return sessions.LockedSession{}, nil
}
func (*deliveryTx) LoadTurn(context.Context) (sessions.Turn, error) {
	return sessions.Turn{ID: "turn", Status: sessions.TurnInProgress}, nil
}
func (tx *deliveryTx) StageArtifacts(context.Context) error { tx.staged = true; return nil }

// Opt-in local media qualification uses files whose codecs/ZIP extraction are
// independently verified by the operator. Regular CI remains self-contained.
func TestArtifactDeliveryMediaFixtureQA(t *testing.T) {
	root := os.Getenv("ARTIFACT_DELIVERY_FIXTURE_DIR")
	if root == "" {
		t.Skip("set ARTIFACT_DELIVERY_FIXTURE_DIR for real-media byte qualification")
	}
	names := []string{"demo.mp4", "preview.png", "source.zip"}
	original := map[string][]byte{}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		original[name] = body
		if err := tw.WriteHeader(&tar.Header{Name: "outputs/" + name, Size: int64(len(body)), Mode: 0600, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	conn := &deliveryConn{archive: archive.Bytes(), incoming: make(chan []byte, 1), closed: make(chan struct{})}
	gateway := runtimegateway.NewSessionWithOwner(conn, "device", "tenant", "test", runtimegateway.NewRegistry(), func(string, ...any) {}, nil)
	gateway.Start()
	defer gateway.Close("test")
	storage := &deliveryStorage{}
	service, err := sessions.NewService(storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = gateway.ExportWorkspaceOutputs(t.Context(), proto.WorkspaceExportPayload{Handle: "prepared", EnvironmentID: "environment"}, func(r io.Reader) error {
		return service.StageTurnArtifacts(t.Context(), sessions.StageTurnArtifactsCommand{TenantID: "tenant", SessionID: "session", TurnID: "turn", EnvironmentID: "environment", Export: r})
	})
	if err != nil || !storage.staged || len(storage.data) != 3 {
		t.Fatalf("capture failed: %v staged=%v", err, storage.staged)
	}
	wantPulls := (len(conn.archive)+proto.WorkspaceExportChunkBytes-1)/proto.WorkspaceExportChunkBytes + 1
	if conn.pulls != wantPulls {
		t.Fatalf("pull count %d want %d", conn.pulls, wantPulls)
	}
	var tenantScope string
	h, _, tenant := testHandler(t, func(_ *Dependencies, f *testFakes) {
		f.artifactsReader.readSessionArtifact = func(_ context.Context, gotTenant, session, id string, consume func(sessions.Artifact, io.Reader) error) error {
			if gotTenant != tenantScope || session != "session" {
				return sessions.ErrNotFound
			}
			body, ok := storage.data["/workspace/outputs/"+id]
			if !ok {
				return sessions.ErrNotFound
			}
			return consume(sessions.Artifact{ID: id, SessionID: session, Path: "/workspace/outputs/" + id, SizeBytes: int64(len(body))}, bytes.NewReader(body))
		}
	})
	tenantScope = tenant
	for _, name := range names {
		request := httptest.NewRequest("GET", "/v1/agents/sessions/session/artifacts/"+name+"/content", nil)
		request.Header.Set("Authorization", "Bearer test-api-key")
		request.Header.Set("OpenAI-Beta", "agents=v1")
		w := &artifactResponseRecorder{httptest.NewRecorder()}
		h.ServeHTTP(w, request)
		got := w.Body.Bytes()
		want := original[name]
		if w.Code != 200 || len(got) != len(want) || sha256.Sum256(got) != sha256.Sum256(want) {
			t.Fatalf("download mismatch %s status=%d bytes=%d want=%d", name, w.Code, len(got), len(want))
		}
		t.Logf("%s bytes=%d sha256=%x", name, len(got), sha256.Sum256(got))
		if name == "source.zip" {
			zr, err := zip.NewReader(bytes.NewReader(got), int64(len(got)))
			if err != nil {
				t.Fatal(err)
			}
			var expanded int64
			for _, file := range zr.File {
				r, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				n, err := io.Copy(io.Discard, r)
				r.Close()
				if err != nil {
					t.Fatal(err)
				}
				expanded += n
			}
			if expanded < 8<<20 {
				t.Fatalf("ZIP expanded only %d bytes", expanded)
			}
		}
	}
	t.Logf("archive bytes=%d pulls=%d, real gateway + Stage + public content GET", len(conn.archive), conn.pulls)
}

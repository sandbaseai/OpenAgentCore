package dispatch

import (
	"bytes"
	"io"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

type qaReadResult struct {
	body  []byte
	err   error
	reads int
}

func (r *qaReadResult) Read(p []byte) (int, error) { r.reads++; return copy(p, r.body), r.err }
func TestWorkspaceExportQAEOFAndNoProgress(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    []byte
		err     error
		outcome string
	}{
		{"empty clean EOF", nil, io.EOF, "completed"},
		{"data and clean EOF", []byte{0, 255, 3}, io.EOF, "chunk"},
		{"no progress", nil, nil, "failed"},
		{"partial unexpected EOF", []byte{0, 255, 3}, io.ErrUnexpectedEOF, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &qaReadResult{body: test.body, err: test.err}
			got := readWorkspaceExportChunk(r, make([]byte, proto.WorkspaceExportChunkBytes), 0)
			if got.Outcome != test.outcome || r.reads != 1 {
				t.Fatalf("outcome=%s reads=%d", got.Outcome, r.reads)
			}
			if test.outcome == "chunk" && !bytes.Equal(got.Data, test.body) {
				t.Fatal("clean tail bytes lost")
			}
			if test.outcome == "failed" && (len(got.Data) != 0 || got.ErrorCode != "export_failed") {
				t.Fatal("failure leaked partial chunk")
			}
		})
	}
}

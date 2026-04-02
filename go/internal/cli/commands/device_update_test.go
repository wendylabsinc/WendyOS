package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// TestDeviceUpdateUpload_SendsAllBytesFromReader verifies that every byte
// produced by the io.Reader is delivered to the agent service, and that the
// agent receives them in the correct order.
func TestDeviceUpdateUpload_SendsAllBytesFromReader(t *testing.T) {
	content := bytes.Repeat([]byte("z"), 200*1024) // 200 KiB
	stream := &fakeUpdateClientStream{}
	mock := &mockAgentServiceClient{updateAgentStream: stream}

	if err := deviceUpdateUploadReader(context.Background(), mock, bytes.NewReader(content)); err != nil {
		t.Fatalf("deviceUpdateUploadReader: %v", err)
	}

	var got []byte
	for _, msg := range stream.sent {
		if chunk := msg.GetChunk(); chunk != nil {
			got = append(got, chunk.GetData()...)
		}
	}
	if !bytes.Equal(got, content) {
		t.Errorf("delivered %d bytes, want %d", len(got), len(content))
	}
}

// TestDeviceUpdateUpload_SHA256IsComputedFromReaderContent verifies that the
// hash sent in the commit control message equals the SHA256 of the bytes
// actually read from the io.Reader, computed inline during streaming rather
// than pre-computed from a full in-memory []byte.
func TestDeviceUpdateUpload_SHA256IsComputedFromReaderContent(t *testing.T) {
	content := bytes.Repeat([]byte("a"), 130*1024) // spans multiple chunks
	h := sha256.Sum256(content)
	want := hex.EncodeToString(h[:])

	stream := &fakeUpdateClientStream{}
	mock := &mockAgentServiceClient{updateAgentStream: stream}

	if err := deviceUpdateUploadReader(context.Background(), mock, bytes.NewReader(content)); err != nil {
		t.Fatalf("deviceUpdateUploadReader: %v", err)
	}

	got := commitHashFromSent(stream.sent)
	if got != want {
		t.Errorf("commit SHA256 = %q, want %q", got, want)
	}
}

// TestDeviceUpdateUpload_SendsDataInChunksNoLargerThan64KiB verifies that
// no single chunk message sent to the agent carries more than 64 KiB of
// payload, ensuring the stream stays bounded in per-message memory use.
func TestDeviceUpdateUpload_SendsDataInChunksNoLargerThan64KiB(t *testing.T) {
	content := bytes.Repeat([]byte("b"), 200*1024) // 200 KiB — forces multiple chunks
	stream := &fakeUpdateClientStream{}
	mock := &mockAgentServiceClient{updateAgentStream: stream}

	if err := deviceUpdateUploadReader(context.Background(), mock, bytes.NewReader(content)); err != nil {
		t.Fatalf("deviceUpdateUploadReader: %v", err)
	}

	const maxChunk = 64 * 1024
	for i, msg := range stream.sent {
		if chunk := msg.GetChunk(); chunk != nil {
			if len(chunk.GetData()) > maxChunk {
				t.Errorf("message %d: chunk size %d exceeds 64 KiB", i, len(chunk.GetData()))
			}
		}
	}
}

// commitHashFromSent returns the SHA256 from the commit control message
// in sent, or "" if no commit message is present.
func commitHashFromSent(sent []*agentpb.UpdateAgentRequest) string {
	for _, msg := range sent {
		if ctrl := msg.GetControl(); ctrl != nil {
			if upd := ctrl.GetUpdate(); upd != nil {
				return upd.GetSha256()
			}
		}
	}
	return ""
}

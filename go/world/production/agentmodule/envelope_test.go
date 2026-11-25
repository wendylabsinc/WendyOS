package agentmodule

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func TestLinuxQueryPreservesLargeAcceptedProofEnvelope(t *testing.T) {
	// setupModule requires actual Linux Unix peer credentials and SO_PEERPIDFD;
	// this exercises publication, durable admission and the real query RPC.
	f := setupModule(t, accessConfig)
	source := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	writer := moduleDial(t, f.mount(t, 0, 19).SocketPath)
	reader := moduleDial(t, accessMount(t, f, source).SocketPath)
	request := moduleRequest("gas")
	var scalar world.ScalarSample
	if err := json.Unmarshal(request.PayloadJson, &scalar); err != nil {
		t.Fatal(err)
	}
	// The scalar schema permits a nonempty calibration revision, and the full
	// payload remains below the publication cap. Nested base64 plus the real
	// certificate/delegation proof makes its returned envelope exceed 64 KiB.
	scalar.CalibrationRevision = strings.Repeat("c", 30000)
	var err error
	request.PayloadJson, err = json.Marshal(scalar)
	if err != nil || len(request.PayloadJson) < 30000 || len(request.PayloadJson) > serviceorigin.MaxPayloadBytes {
		t.Fatal("fixture did not produce a valid near-limit scalar payload", err)
	}
	accepted := modulePublish(t, writer, request)
	if len(accepted.SignedRecordJson) <= serviceorigin.MaxRecordBytes || len(accepted.SignedRecordJson) >= serviceaccess.MaxQueryBytes {
		t.Fatalf("fixture proof envelope is outside the intended (64,192) KiB range: %d bytes", len(accepted.SignedRecordJson))
	}
	_, record := decodeReply(t, accepted)
	if !bytes.Equal(record.Publication.Payload, request.PayloadJson) || record.Scope.Service != source.Service || record.Publication.Stream != source.Stream {
		t.Fatal("durable publication changed payload or authenticated source")
	}
	page := accessQuery(t, reader, []serviceaccess.Source{source}, 1, "")
	if len(page.SignedRecordJson) != 1 || page.Next != "" || !bytes.Equal(page.SignedRecordJson[0], accepted.SignedRecordJson) {
		t.Fatal("query refused, truncated or reconstructed the large original proof")
	}
	t.Logf("preserved %d-byte publication payload and %d-byte original proof envelope", len(request.PayloadJson), len(accepted.SignedRecordJson))
}

package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/executor"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

func TestInspectionEvidenceRequiresLocalCompleteBytesAndReusesPublisherOwnership(t *testing.T) {
	f := newFixture(t, nil, "capture-node")
	blobs, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "tenant", Site: "office"})
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	f.n.cfg.Evidence = blobs
	cred := evidenceCredentials(t, f, "sensor", "capture-node", "evidence-write", "evidence-read")
	sensor, err := client.New(client.Config{URL: f.server.URL, Audience: f.audience, Grant: cred.Grant, PrivateKey: cred.PrivateKey, AllowInsecureSimulation: true})
	if err != nil {
		t.Fatal(err)
	}
	cred = evidenceCredentials(t, f, "executor", "capture-node", "evidence-write", "evidence-read")
	worker, err := client.New(client.Config{URL: f.server.URL, Audience: f.audience, Grant: cred.Grant, PrivateKey: cred.PrivateKey, AllowInsecureSimulation: true})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("captured pixels")
	sum := sha256.Sum256(data)
	ref := sensing.EvidenceRef{SHA256: hex.EncodeToString(sum[:]), ContentType: "image/png", Size: int64(len(data))}
	inspection := tasking.Inspection{TaskID: "inspection", RunID: "run", Samples: []sensing.Sample{{Evidence: &ref}}}
	validate := InspectionEvidence(blobs)
	if validate(inspection) == nil {
		t.Fatal("missing evidence accepted")
	}
	expiry := time.Now().Add(time.Hour)
	metadata, err := sensor.UploadEvidence(context.Background(), data, ref.ContentType, expiry)
	if err != nil {
		t.Fatal(err)
	}
	if err = validate(inspection); err != nil {
		t.Fatal(err)
	}
	gateway := executor.NodeGateway{Client: worker}
	if err = gateway.StoreEvidence(context.Background(), ref, data, expiry); err != nil {
		t.Fatal("publisher-owned bytes not reused", err)
	}
	existing, err := worker.GetEvidence(context.Background(), evidence.GetRequest{SHA256: ref.SHA256, Length: 1})
	if err != nil {
		t.Fatal(err)
	}
	if existing.RetainUntilUnixMS != metadata.RetainUntilUnixMS {
		t.Fatal("reuse modified retention")
	}
	ref.Size++
	if validate(inspection) == nil {
		t.Fatal("wrong size accepted")
	}
	ref.Size--
	ref.ContentType = "audio/wav"
	if validate(inspection) == nil {
		t.Fatal("wrong content type accepted")
	}
	if InspectionEvidence(nil)(inspection) == nil {
		t.Fatal("missing store accepted")
	}
}
func TestInspectionAdmissionRequiresEvidenceStore(t *testing.T) {
	f, op, _, _, _ := taskFixture(t)
	spec := taskSpec("inspect")
	spec.Type = tasking.InspectType
	code, _ := call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", spec))
	if code == 200 {
		t.Fatal("inspection admitted without evidence")
	}
}

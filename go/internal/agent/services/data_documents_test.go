package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func apply(t *testing.T, service *DataService, contents string) (*agentpbv2.DataDocument, error) {
	t.Helper()
	response, err := service.DocumentApply(context.Background(), &agentpbv2.DataDocumentApplyRequest{DocumentYaml: []byte(contents)})
	return response.GetDocument(), err
}

func mustApply(t *testing.T, service *DataService, contents string) *agentpbv2.DataDocument {
	t.Helper()
	document, err := apply(t, service, contents)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func ownedInference(name, owner string) string {
	return strings.Replace(testInferenceDocumentYAML, "name: front-door", "name: "+name+"\nowner: "+owner, 1)
}

func TestDocumentApplyOwnership(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	if _, err := apply(t, service, ownedInference("chat-1", "watch/chat-1")); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("owner missing: %v", err)
	}
	watch := mustApply(t, service, watchYAML("chat-1", "chat-1", "lease: 60s\n"))
	if watch.GetKind() != "watch" || watch.GetExpiresUnixNanos() == 0 || watch.GetState() != "waiting" {
		t.Fatalf("watch = %+v", watch)
	}
	inference := mustApply(t, service, ownedInference("chat-1", "watch/chat-1"))
	if inference.GetOwner() != "watch/chat-1" {
		t.Fatalf("inference = %+v", inference)
	}
	if _, err := apply(t, service, watchYAML("nested", "x", "owner: inference/chat-1\n")); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "one level") {
		t.Fatalf("owner with an owner: %v", err)
	}
	mustApply(t, service, watchYAML("other", "x", ""))
	if _, err := apply(t, service, ownedInference("chat-1", "watch/other")); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "cannot change") {
		t.Fatalf("owner change: %v", err)
	}
	if _, err := apply(t, service, watchYAML("chat-1", "chat-1", "")); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("lease dropped: %v", err)
	}
	mustApply(t, service, watchYAML("porch", "front-door", ""))
	if _, err := apply(t, service, watchYAML("porch", "front-door", "lease: 60s\n")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("lease added: %v", err)
	}
	if _, err := apply(t, service, "kind: watch\nversion: 1\nname: x\non: {inference: \"\"}\n"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid document: %v", err)
	}
}

func TestDocumentApplyCollectionTakesTheCampaignPath(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	document := mustApply(t, service, testClipsCollection)
	if document.GetKind() != "collection" || document.GetName() != "clips" || document.GetState() != "armed" || len(document.GetDocumentJson()) == 0 {
		t.Fatalf("collection = %+v", document)
	}
	if _, err := service.manager.Campaign("clips"); err != nil {
		t.Fatalf("the collection is not a campaign: %v", err)
	}
}

func TestDocumentApplyWithoutRuntime(t *testing.T) {
	service, _, _ := newDocumentTestService(t)
	if _, err := apply(t, service, watchYAML("porch", "front-door", "")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no runtime: %v", err)
	}
}

func TestDocumentRemoveStopsOwnedInferenceBeforeReturning(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	mustApply(t, service, watchYAML("chat-1", "chat-1", "lease: 60s\n"))
	mustApply(t, service, ownedInference("chat-1", "watch/chat-1"))
	session := receiveInference(t, factory.sessions)
	response, err := service.DocumentRemove(context.Background(), &agentpbv2.DataDocumentRemoveRequest{Kind: "watch", Name: "chat-1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(response.GetRemoved(), ",") != "inference/chat-1,watch/chat-1" {
		t.Fatalf("removed = %v", response.GetRemoved())
	}
	select {
	case <-session.closed:
	default:
		t.Fatal("DocumentRemove returned before the inference stopped")
	}
	service.inference.wakeNow()
	select {
	case <-factory.sessions:
		t.Fatal("a later pass restarted the removed inference")
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := service.DocumentRenew(context.Background(), &agentpbv2.DataDocumentRenewRequest{Kind: "watch", Name: "chat-1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("renew after removal: %v", err)
	}
	for kind, want := range map[string]codes.Code{"watch": codes.NotFound, "collection": codes.FailedPrecondition, "robot": codes.InvalidArgument} {
		if _, err := service.DocumentRemove(context.Background(), &agentpbv2.DataDocumentRemoveRequest{Kind: kind, Name: "chat-1"}); status.Code(err) != want {
			t.Fatalf("remove %s: %v, want %v", kind, err, want)
		}
	}
}

func TestDocumentRenew(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	clock := newFakeClock()
	service.leases.now = clock.Now
	startDocumentTestService(t, service, factory, sender)
	mustApply(t, service, watchYAML("chat-1", "chat-1", "lease: 60s\n"))
	mustApply(t, service, watchYAML("porch", "front-door", ""))
	mustApply(t, service, testInferenceDocumentYAML)
	clock.Add(30 * time.Second)
	renewed, err := service.DocumentRenew(context.Background(), &agentpbv2.DataDocumentRenewRequest{Kind: "watch", Name: "chat-1"})
	if err != nil || renewed.GetExpiresUnixNanos() != clock.Now().Add(time.Minute).UnixNano() {
		t.Fatalf("renew = %v, %v", renewed, err)
	}
	for _, request := range []*agentpbv2.DataDocumentRenewRequest{{Kind: "watch", Name: "porch"}, {Kind: "inference", Name: "front-door"}} {
		if _, err := service.DocumentRenew(context.Background(), request); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("renew %v: %v", request, err)
		}
	}
	clock.Add(61 * time.Second)
	if _, err := service.DocumentRenew(context.Background(), &agentpbv2.DataDocumentRenewRequest{Kind: "watch", Name: "chat-1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("renew after the deadline: %v", err)
	}
}

func TestDocumentsReportsStatus(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	mustApply(t, service, testClipsCollection)
	mustApply(t, service, watchYAML("porch", "front-door", ""))
	list := func(kind, name string) []*agentpbv2.DataDocument {
		t.Helper()
		response, err := service.Documents(context.Background(), &agentpbv2.DataDocumentsRequest{Kind: kind, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return response.GetDocuments()
	}
	if porch := list("watch", "porch"); len(porch) != 1 || porch[0].GetState() != "waiting" || !strings.Contains(porch[0].GetStateDetail(), "front-door") {
		t.Fatalf("waiting watch = %+v", porch)
	}
	mustApply(t, service, testInferenceDocumentYAML)
	session := receiveInference(t, factory.sessions)
	receiveInference(t, session.inputs)
	eventually(t, "the watch to become active", func() bool { return list("watch", "porch")[0].GetState() == "active" })
	all := list("", "")
	var kinds []string
	for _, document := range all {
		kinds = append(kinds, document.GetKind()+"/"+document.GetName())
	}
	if strings.Join(kinds, ",") != "collection/clips,inference/front-door,watch/porch" {
		t.Fatalf("all documents = %v", kinds)
	}
	if _, err := service.Documents(context.Background(), &agentpbv2.DataDocumentsRequest{Kind: "watch", Name: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing document: %v", err)
	}
	if _, err := service.Documents(context.Background(), &agentpbv2.DataDocumentsRequest{Kind: "robot"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown kind: %v", err)
	}
	_ = data.KindWatch
}

func TestInferenceDocumentStatus(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	applied := mustApply(t, service, testInferenceDocumentYAML)
	if applied.GetState() != "pending" && applied.GetState() != "loading" && applied.GetState() != "running" {
		t.Fatalf("new enabled inference state = %q", applied.GetState())
	}
	mustApply(t, service, strings.Replace(testInferenceDocumentYAML, "name: front-door", "name: off", 1)+"enabled: false\n")
	response, err := service.Documents(context.Background(), &agentpbv2.DataDocumentsRequest{Kind: "inference", Name: "off"})
	if err != nil || response.GetDocuments()[0].GetState() != "disabled" {
		t.Fatalf("disabled inference = %v, %v", response, err)
	}
	service.inference = nil
	stored, err := service.manager.Document(data.KindInference, "front-door")
	if err != nil {
		t.Fatal(err)
	}
	if message := service.documentMessage(stored); message.GetState() != "error" || !strings.Contains(message.GetStateDetail(), "unavailable") {
		t.Fatalf("no runtime = %+v", message)
	}
}

func TestWatchReturnsToWaitingAfterItsInferenceIsRemoved(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	mustApply(t, service, watchYAML("porch", "front-door", ""))
	mustApply(t, service, testInferenceDocumentYAML)
	session := receiveInference(t, factory.sessions)
	receiveInference(t, session.inputs)
	watch := func() *agentpbv2.DataDocument {
		t.Helper()
		response, err := service.Documents(context.Background(), &agentpbv2.DataDocumentsRequest{Kind: "watch", Name: "porch"})
		if err != nil {
			t.Fatal(err)
		}
		return response.GetDocuments()[0]
	}
	eventually(t, "the watch to become active", func() bool { return watch().GetState() == "active" })
	if _, err := service.DocumentRemove(context.Background(), &agentpbv2.DataDocumentRemoveRequest{Kind: "inference", Name: "front-door"}); err != nil {
		t.Fatal(err)
	}
	if got := watch(); got.GetState() != "waiting" || !strings.Contains(got.GetStateDetail(), "front-door") {
		t.Fatalf("watch after its inference was removed = %s (%s)", got.GetState(), got.GetStateDetail())
	}
}

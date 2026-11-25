package node

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

type proposalReviewFixture struct {
	f               *fixture
	agent, executor Credentials
	path            string
	cfg             proposals.Config
	now             atomic.Int64
}

func newProposalReviewFixture(t *testing.T) *proposalReviewFixture {
	t.Helper()
	f, _, ex, _, taskCfg := taskFixture(t)
	if err := f.n.cfg.Tasks.Close(); err != nil {
		t.Fatal(err)
	}
	taskCfg.RunID = "review-run"
	tasks, err := tasking.Open(filepath.Join(t.TempDir(), "tasks.json"), taskCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tasks.Close() })
	f.n.cfg.Tasks = tasks
	f.n.cfg.Audience = f.audience
	r := &proposalReviewFixture{f: f, executor: ex, path: filepath.Join(t.TempDir(), "proposals.jsonl")}
	r.now.Store(time.Now().Add(100 * time.Millisecond).UnixMilli())
	r.cfg = proposals.Config{Tenant: "tenant", Site: "office", RunID: "review-run", Audience: f.audience, Assignee: "robot/navigation", Catalog: taskCfg.Catalog, Now: func() time.Time {
		now := time.Now()
		if pinned := time.UnixMilli(r.now.Load()); pinned.After(now) {
			return pinned
		}
		return now
	}}
	ps, err := proposals.Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	f.n.cfg.Proposals = ps
	if err = f.n.configureProposals(); err != nil {
		t.Fatal(err)
	}
	r.agent = missionCredentials(t, f, "agent", "review-agent", "", "read", "propose")
	return r
}

func (r *proposalReviewFixture) request(id string) proposals.Request {
	return proposals.Request{Version: 1, ID: id, Tenant: "tenant", Site: "office", RunID: "review-run", Assignee: "robot/navigation", Type: "wendy.nav.go_to.v1", Parameters: json.RawMessage(`{"target":[0.5,0,0],"class":"OBSERVE","instruction":"send now"}`), Summary: "Untrusted proposal text; review before execution.", NotAfterUnixMS: time.Now().Add(10 * time.Minute).UnixMilli()}
}

func reviewProposalPost(t *testing.T, r *proposalReviewFixture, credentials Credentials, body []byte) (int, []byte) {
	t.Helper()
	return call(t, r.f, credentials, http.MethodPost, "/v1/proposals", "propose", body)
}

func TestReviewProposalCannotReachTaskStoreOrExecutor(t *testing.T) {
	r := newProposalReviewFixture(t)
	epoch := startExecutor(t, r.f, r.executor)
	body := jsonBody(t, r.request("review-motion"))
	code, reply := reviewProposalPost(t, r, r.agent, body)
	if code != http.StatusCreated {
		t.Fatalf("proposal: %d %s", code, reply)
	}
	var view proposals.View
	if err := json.Unmarshal(reply, &view); err != nil {
		t.Fatal(err)
	}
	if view.AdapterValidated || view.Expired || view.Record.Class != mission.Move || view.Record.Kind != "agent" || !bytes.Equal(view.Record.Body, body) {
		t.Fatal("proposal invented adapter validation, class downgrade, or changed original body")
	}
	hash := sha256.Sum256(body)
	if view.Record.BodySHA256 != hex.EncodeToString(hash[:]) || view.Record.Proof.BodySHA256 != view.Record.BodySHA256 || view.Record.Source != r.agent.Grant.Grant.Source || view.Record.Principal != r.agent.Grant.Grant.Principal || view.Record.GrantID != r.agent.Grant.Grant.ID {
		t.Fatal("retained proof/source/body binding changed")
	}
	verifyAt := time.UnixMilli(view.Record.AcceptedAtUnixMS)
	verify := func(proof authority.SignedRequest, body []byte) error {
		v, err := authority.NewVerifier(authority.Config{RootPublicKey: r.f.root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: r.f.audience, Now: func() time.Time { return verifyAt }, Replay: authority.NewMemoryReplay(10)})
		if err != nil {
			return err
		}
		rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: verifyAt.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: verifyAt.Add(time.Minute).UnixMilli()}, r.f.root)
		if err != nil {
			return err
		}
		if err = v.UpdateRevocations(rev); err != nil {
			return err
		}
		_, err = v.Verify(proof, "propose", body)
		return err
	}
	if err := verify(view.Record.Proof, body); err != nil {
		t.Fatal("retained proof no longer verifies", err)
	}
	if err := verify(view.Record.Proof, append([]byte(" "), body...)); err == nil {
		t.Fatal("retained proof accepted changed original bytes")
	}
	changed := view.Record.Proof
	changed.Grant.Grant.Source.Stream = "other-stream"
	if err := verify(changed, body); err == nil {
		t.Fatal("retained proof accepted a different source")
	}
	if _, err := r.f.n.cfg.Tasks.Get("review-motion"); err == nil {
		t.Fatal("proposal became a task")
	}
	code, reply = call(t, r.f, r.executor, http.MethodPost, "/v1/tasks/evaluate", "execute", jsonBody(t, EvaluateRequest{TaskID: "review-motion", ExecutorEpoch: epoch, Telemetry: telemetry()}))
	if code >= 200 && code < 300 {
		t.Fatalf("executor evaluated proposal as work: %s", reply)
	}
	for _, target := range []struct{ path, operation string }{{"/v1/tasks", "mission"}, {"/v1/tasks/stop", "stop"}, {"/v1/tasks/executor/start", "execute"}, {"/v1/tasks/report", "execute"}} {
		code, _ = call(t, r.f, r.agent, http.MethodPost, target.path, target.operation, body)
		if code != http.StatusUnauthorized {
			t.Fatalf("agent reached %s: %d", target.path, code)
		}
	}
	list, err := r.f.n.cfg.Tasks.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("proposal/privilege attempts changed task store: %d %v", len(list), err)
	}
	code, _ = call(t, r.f, r.agent, http.MethodGet, "/v1/tasks/status?task_id=review-motion", "read", nil)
	if code != http.StatusNotFound {
		t.Fatal("proposal exposed as task status", code)
	}
}

func TestReviewExpiredGrantRetryCannotReuseRetainedProposal(t *testing.T) {
	r := newProposalReviewFixture(t)
	body := jsonBody(t, r.request("grant-expiry"))
	if code, b := reviewProposalPost(t, r, r.agent, body); code != http.StatusCreated {
		t.Fatal(code, string(b))
	}
	// Advance only the trusted verifier clock and sign a fresh transport request
	// at that time. This distinguishes grant expiry from stale HTTP envelopes.
	now := time.UnixMilli(r.agent.Grant.Grant.NotAfterUnixMS + 1)
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: r.f.root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: r.f.audience, Now: func() time.Time { return now }, Replay: authority.NewMemoryReplay(10)})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli()}, r.f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	r.f.n.verifier = v
	proof, err := authority.SignRequest(r.agent.Grant, r.agent.PrivateKey, r.f.audience, "propose", body, "review_expired_grant_123", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, r.f.server.URL+"/v1/proposals", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(AuthorizationHeader, base64.StdEncoding.EncodeToString(jsonBody(t, proof)))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("expired grant reused retained proposal", response.StatusCode)
	}
	page, err := r.f.n.cfg.Proposals.List(100, "")
	if err != nil || len(page.Proposals) != 1 {
		t.Fatal("expired grant changed retained history", err)
	}
}

func TestReviewProposalExactRetrySurvivesRestartWithoutNewAuthority(t *testing.T) {
	r := newProposalReviewFixture(t)
	body := jsonBody(t, r.request("immutable"))
	code, first := reviewProposalPost(t, r, r.agent, body)
	if code != http.StatusCreated {
		t.Fatal(code, string(first))
	}
	code, second := reviewProposalPost(t, r, r.agent, body)
	if code != http.StatusOK || !bytes.Equal(first, second) {
		t.Fatal("retry changed first proposal, expiry or proof")
	}
	if err := r.f.n.cfg.Proposals.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := proposals.Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r.f.n.cfg.Proposals = reopened
	code, third := reviewProposalPost(t, r, r.agent, body)
	if code != http.StatusOK || !bytes.Equal(first, third) {
		t.Fatal("restart changed immutable proposal")
	}
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) { v["summary"] = "replacement" },
		func(v map[string]any) { v["not_after_unix_ms"] = float64(time.Now().Add(20 * time.Minute).UnixMilli()) },
		func(v map[string]any) { v["parameters"] = map[string]any{"target": []int{9, 9, 9}} },
	} {
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		mutate(v)
		if code, _ = reviewProposalPost(t, r, r.agent, jsonBody(t, v)); code != http.StatusConflict {
			t.Fatal("ID reuse accepted changed body", code)
		}
	}
	other := missionCredentials(t, r.f, "agent", "other-agent", "", "read", "propose")
	if code, _ = reviewProposalPost(t, r, other, body); code != http.StatusConflict {
		t.Fatal("another proposer reused ID", code)
	}
	changedSource := r.agent
	g := changedSource.Grant.Grant
	g.Source.Stream = "different-stream"
	changedSource.Grant, err = authority.SignGrant(g, r.f.root)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ = reviewProposalPost(t, r, changedSource, body); code != http.StatusConflict {
		t.Fatal("same principal with a different authorized source reused ID", code)
	}
	renewed := r.agent
	g = renewed.Grant.Grant
	g.ID = "renewed-agent-grant"
	renewed.Grant, err = authority.SignGrant(g, r.f.root)
	if err != nil {
		t.Fatal(err)
	}
	code, renewedReply := reviewProposalPost(t, r, renewed, body)
	if code != http.StatusOK || !bytes.Equal(first, renewedReply) {
		t.Fatal("renewed current grant rewrote the original historical proof")
	}
	// JSON whitespace is part of the authenticated exact body, not a license to
	// replace the stored bytes under an existing ID.
	if code, _ = reviewProposalPost(t, r, r.agent, append([]byte(" "), body...)); code != http.StatusConflict {
		t.Fatal("exact-body ID binding lost", code)
	}
	if list, err := reopened.List(100, ""); err != nil || len(list.Proposals) != 1 {
		t.Fatal("conflicts added records", err)
	}
}

func TestReviewProposalExactFieldsAndScopeRefuseBeforePersistence(t *testing.T) {
	r := newProposalReviewFixture(t)
	base := r.request("invalid")
	mutations := map[string]func(map[string]any){
		"wrong_tenant":       func(v map[string]any) { v["tenant"] = "other" },
		"wrong_site":         func(v map[string]any) { v["site"] = "other" },
		"wrong_run":          func(v map[string]any) { v["run_id"] = "other" },
		"wrong_assignee":     func(v map[string]any) { v["assignee"] = "other/navigation" },
		"undeclared_type":    func(v map[string]any) { v["type"] = "wendy.arm.actuate.v1" },
		"class_override":     func(v map[string]any) { v["class"] = "OBSERVE" },
		"operator_signature": func(v map[string]any) { v["signature"] = "not an operator decision" },
		"wrong_case":         func(v map[string]any) { v["Version"] = v["version"]; delete(v, "version") },
		"case_alias":         func(v map[string]any) { v["ID"] = "different" },
		"null_summary":       func(v map[string]any) { v["summary"] = nil },
		"missing_summary":    func(v map[string]any) { delete(v, "summary") },
		"null_parameters":    func(v map[string]any) { v["parameters"] = nil },
		"array_parameters":   func(v map[string]any) { v["parameters"] = []any{} },
		"oversized_parameters": func(v map[string]any) {
			v["parameters"] = map[string]any{"text": strings.Repeat("x", proposals.MaxParametersBytes)}
		},
		"oversized_summary": func(v map[string]any) { v["summary"] = strings.Repeat("界", 171) },
		"expired":           func(v map[string]any) { v["not_after_unix_ms"] = time.Now().Add(-time.Second).UnixMilli() },
		"after_grant":       func(v map[string]any) { v["not_after_unix_ms"] = r.agent.Grant.Grant.NotAfterUnixMS + 1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			_ = json.Unmarshal(jsonBody(t, base), &v)
			mutate(v)
			code, b := reviewProposalPost(t, r, r.agent, jsonBody(t, v))
			if code >= 200 && code < 300 {
				t.Fatalf("invalid proposal accepted: %s", b)
			}
		})
	}
	for _, raw := range []string{`{"target":1,"target":2}`, `{"nested":{"class":1,"class":2}}`} {
		q := base
		q.Parameters = json.RawMessage(raw)
		if code, _ := reviewProposalPost(t, r, r.agent, jsonBody(t, q)); code != http.StatusBadRequest {
			t.Fatal("duplicate nested parameters accepted", code)
		}
	}
	page, err := r.f.n.cfg.Proposals.List(100, "")
	if err != nil || len(page.Proposals) != 0 {
		t.Fatal("refused input persisted", err)
	}
}

func TestReviewProposalRevocationAndExpiryDoNotAllowIdempotentBypass(t *testing.T) {
	r := newProposalReviewFixture(t)
	body := jsonBody(t, r.request("revoked-retry"))
	if code, b := reviewProposalPost(t, r, r.agent, body); code != http.StatusCreated {
		t.Fatal(code, string(b))
	}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), GrantIDs: []string{r.agent.Grant.Grant.ID}}, r.f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.f.v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	if code, _ := reviewProposalPost(t, r, r.agent, body); code != http.StatusUnauthorized {
		t.Fatal("revoked retry bypassed authority", code)
	}
	newAgent := missionCredentials(t, r.f, "agent", "fresh-agent", "", "read", "propose")
	expired := r.request("expired-retry")
	expired.NotAfterUnixMS = now.Add(time.Minute).UnixMilli()
	body = jsonBody(t, expired)
	if code, b := reviewProposalPost(t, r, newAgent, body); code != http.StatusCreated {
		t.Fatal(code, string(b))
	}
	r.now.Store(now.Add(2 * time.Minute).UnixMilli())
	if code, _ := reviewProposalPost(t, r, newAgent, body); code != http.StatusUnauthorized {
		t.Fatal("expired retained proposal refreshed by retry", code)
	}
	v, err := r.f.n.cfg.Proposals.Get(expired.ID)
	if err != nil || !v.Expired || v.AdapterValidated || v.Record.Proposal.NotAfterUnixMS != expired.NotAfterUnixMS {
		t.Fatal("historical view changed expiry/authority", err)
	}
}

func TestReviewProposalRoleAndReadPaginationBoundaries(t *testing.T) {
	r := newProposalReviewFixture(t)
	body := jsonBody(t, r.request("role"))
	for _, kind := range []string{"app", "device"} {
		credential := missionCredentials(t, r.f, kind, "role-"+kind, "", "read", "propose")
		if code, _ := reviewProposalPost(t, r, credential, body); code != http.StatusForbidden {
			t.Fatal("non-proposer role accepted", kind, code)
		}
	}
	reader := missionCredentials(t, r.f, "agent", "read-only", "", "read")
	if code, _ := reviewProposalPost(t, r, reader, body); code != http.StatusUnauthorized {
		t.Fatal("read grant persisted proposal", code)
	}
	for _, path := range []string{"/v1/proposals?limit=101", "/v1/proposals?limit=1&limit=2", "/v1/proposals?limit=", "/v1/proposals?after=../x", "/v1/proposals?gateway=other", "/v1/tasks/catalog?url=other", "/v1/tasks/status?task_id=a&task_id=b"} {
		if code, _ := call(t, r.f, r.agent, http.MethodGet, path, "read", nil); code != http.StatusBadRequest {
			t.Fatal("invalid query accepted", path, code)
		}
	}
	code, b := call(t, r.f, r.agent, http.MethodGet, "/v1/tasks/catalog", "read", nil)
	if code != http.StatusOK {
		t.Fatal(code, string(b))
	}
	var catalog tasking.Catalog
	if err := json.Unmarshal(b, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Assignee != "robot/navigation" || catalog.RunID != "review-run" || !reflect.DeepEqual(catalog.Types, []tasking.CatalogEntry{{Type: "wendy.nav.go_to.v1", Class: mission.Move}}) {
		t.Fatal("catalog did not preserve trusted existing wire values")
	}
}

func TestReviewProposalSignedGrantScopeAndOriginalBodyAreRequired(t *testing.T) {
	r := newProposalReviewFixture(t)
	body := jsonBody(t, r.request("signed-scope"))
	for _, field := range []string{"tenant", "site", "domain"} {
		t.Run(field, func(t *testing.T) {
			credentials := r.agent
			g := credentials.Grant.Grant
			switch field {
			case "tenant":
				g.Tenant = "another-tenant"
			case "site":
				g.Site = "another-site"
			case "domain":
				g.Domain = "physical"
			}
			var err error
			credentials.Grant, err = authority.SignGrant(g, r.f.root)
			if err != nil {
				t.Fatal(err)
			}
			if code, _ := reviewProposalPost(t, r, credentials, body); code != http.StatusUnauthorized {
				t.Fatal("wrong signed scope accepted", code)
			}
		})
	}
	for _, field := range []string{"audience", "body"} {
		t.Run(field, func(t *testing.T) {
			audience := r.f.audience
			actualBody := body
			if field == "audience" {
				audience = "other-gateway"
			} else {
				actualBody = append([]byte(" "), body...)
			}
			proof, err := authority.SignRequest(r.agent.Grant, r.agent.PrivateKey, audience, "propose", body, "review_scope_nonce_"+field, time.Now(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, r.f.server.URL+"/v1/proposals", bytes.NewReader(actualBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(AuthorizationHeader, base64.StdEncoding.EncodeToString(jsonBody(t, proof)))
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatal("unbound signed request accepted", response.StatusCode)
			}
		})
	}
	page, err := r.f.n.cfg.Proposals.List(100, "")
	if err != nil || len(page.Proposals) != 0 {
		t.Fatal("invalid signed scope/body changed proposal store", err)
	}
}

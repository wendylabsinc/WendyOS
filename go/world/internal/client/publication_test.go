package client

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// The handler authenticates and accepts the first POST, then closes the socket
// before a response. A second request must have a fresh outer nonce but the
// exact accepted origin record, including its original proof deadline.
func TestPreparedPublicationRetriesAcceptedRecordAfterResponseLoss(t *testing.T) {
	cfg, verifier := credentials(t)
	var bodies [][]byte
	var nonces []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		header, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		if err != nil {
			t.Error(err)
			return
		}
		var auth authority.SignedRequest
		if err = json.Unmarshal(header, &auth); err != nil {
			t.Error(err)
			return
		}
		if _, err = verifier.Verify(auth, "publish", body); err != nil {
			t.Error(err)
			http.Error(w, "denied", 401)
			return
		}
		var record authority.SignedRecord
		if err = json.Unmarshal(body, &record); err != nil {
			t.Error(err)
			return
		}
		if _, err = verifier.VerifyRecord(record); err != nil {
			t.Error(err)
			http.Error(w, "denied", 401)
			return
		}
		bodies = append(bodies, body)
		nonces = append(nonces, auth.Nonce)
		if len(bodies) == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg.URL = server.URL
	publisher, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	component := world.Component{Key: world.Key{Tenant: "t", Site: "s", Entity: "hazard", Type: "hazard", Source: cfg.Grant.Grant.Source}, Version: world.Version{Epoch: 7, Sequence: 19}, Timing: world.Timing{SampleTime: time.Now(), ClockDomain: "simulation:run"}, TTL: 5 * time.Second, Payload: json.RawMessage(`{"hazard":true}`)}
	prepared, err := publisher.PreparePublication(component)
	if err != nil {
		t.Fatal(err)
	}
	if err = publisher.PublishPrepared(context.Background(), prepared); !Transient(err) {
		t.Fatalf("lost response not retryable: %v", err)
	}
	component.Payload[2] = 'X'
	component.Version.Sequence++
	component.TTL = time.Hour
	if err = publisher.PublishPrepared(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) || nonces[0] == nonces[1] {
		t.Fatal("retry changed origin bytes or reused transport nonce")
	}
	var accepted authority.SignedRecord
	if err = json.Unmarshal(bodies[1], &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Component.Version != (world.Version{Epoch: 7, Sequence: 19}) || accepted.Component.TTL != 5*time.Second || string(accepted.Component.Payload) != `{"hazard":true}` {
		t.Fatal("caller mutated prepared origin")
	}
}

func TestPreparedPublicationBoundToPublisher(t *testing.T) {
	cfg, _ := credentials(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer server.Close()
	cfg.URL = server.URL
	publisher, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	component := world.Component{Key: world.Key{Tenant: "t", Site: "s", Source: cfg.Grant.Grant.Source}, TTL: time.Second}
	prepared, err := publisher.PreparePublication(component)
	if err != nil {
		t.Fatal(err)
	}
	otherConfig := cfg
	otherConfig.Audience = "another-node"
	other, err := New(otherConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.PublishPrepared(context.Background(), prepared); err == nil {
		t.Fatal("cross-audience prepared publication accepted")
	}
	otherConfig = cfg
	otherConfig.Grant.Grant.ID = "another-grant"
	other, err = New(otherConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.PublishPrepared(context.Background(), prepared); err == nil {
		t.Fatal("cross-grant prepared publication accepted")
	}
	if err = publisher.PublishPrepared(context.Background(), PreparedPublication{}); err == nil {
		t.Fatal("empty prepared publication accepted")
	}
	if requests != 0 {
		t.Fatal("invalid preparation reached network")
	}
}

func TestTransientAvailabilityClassification(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 507} {
		if !Transient(&HTTPStatusError{StatusCode: status}) {
			t.Fatalf("status %d should retry", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 409, 422} {
		if Transient(&HTTPStatusError{StatusCode: status}) {
			t.Fatalf("status %d should terminate", status)
		}
	}
	for _, err := range []error{context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("unreachable")}, &url.Error{Op: "Post", URL: "http://node", Err: io.EOF}} {
		if !Transient(err) {
			t.Fatalf("transport should retry: %v", err)
		}
	}
	for _, err := range []error{nil, context.Canceled, errors.New("invalid lineage"), &url.Error{Op: "Post", URL: "http://node", Err: errors.New("node redirects are forbidden")}, &url.Error{Op: "Post", URL: "https://node", Err: x509.UnknownAuthorityError{}}} {
		if Transient(err) {
			t.Fatalf("terminal error would loop: %v", err)
		}
	}
}

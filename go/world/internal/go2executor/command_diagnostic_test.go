package go2executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestHTTPCommandReceiptExactAndClearedAfterFailure(t *testing.T) {
	mode := "valid"
	request := CommandRequest{Session: "session", Sequence: 7, CaptureSequence: 12, TaskID: "task", SpecDigest: "spec", ExecutorEpoch: 2, NotAfterUnixMS: 1_000_000, BodyVX: .05}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if mode == "refused" {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"bridge_refused","reason":"command_source_nonadvancing"}`))
			return
		}
		ack := BridgeCommandAcknowledgment{Accepted: true, Sequence: request.Sequence, NotAfterUnixMS: request.NotAfterUnixMS, SourceTimestampNS: (request.NotAfterUnixMS - 450) * int64(time.Millisecond)}
		if mode == "wrong-sequence" {
			ack.Sequence++
		}
		_ = json.NewEncoder(w).Encode(ack)
	}))
	defer server.Close()
	a, _ := NewHTTPAdapter(server.URL, strings.Repeat("a", 32))
	if _, ok := a.LastCommandReceipt(); ok {
		t.Fatal("receipt existed before command")
	}
	if err := a.Command(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	receipt, ok := a.LastCommandReceipt()
	if !ok || receipt.Request != request || !acknowledgmentMatches(receipt.Acknowledgment, request) || receipt.RequestStartedUnixNS <= 0 || receipt.ResponseCompletedUnixNS < receipt.RequestStartedUnixNS || receipt.RoundTripElapsedNS < 0 {
		t.Fatal("validated receipt or actual call timing missing")
	}
	receipt.Request.CaptureSequence = 999
	again, _ := a.LastCommandReceipt()
	if again.Request.CaptureSequence != request.CaptureSequence {
		t.Fatal("receipt aliases caller")
	}
	for _, bad := range []string{"wrong-sequence", "refused"} {
		mode = bad
		if err := a.Command(context.Background(), request); err == nil {
			t.Fatal("invalid command response accepted")
		}
		if _, ok := a.LastCommandReceipt(); ok {
			t.Fatal("prior valid receipt survived a failed command")
		}
	}
}

func TestHTTPRefusalUsesOnlyFixedReason(t *testing.T) {
	for _, test := range []struct{ body, reason string }{
		{`{"error":"bridge_refused","reason":"command_source_nonadvancing"}`, "command_source_nonadvancing"},
		{`{"error":"bridge_refused","reason":"publication_completed_late"}`, "publication_completed_late"},
		{`{"error":"request_refused"}`, ""},
		{`{"error":"bridge_refused","reason":"private-token-contents"}`, ""},
		{`{"error":"bridge_refused","reason":"runtime_unavailable","extra":"secret"}`, ""},
		{`{"error":"bridge_refused","reason":"runtime_unavailable","reason":"clock_continuity"}`, ""},
		{strings.Repeat("secret", 200), ""},
	} {
		t.Run(test.body[:min(len(test.body), 48)], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			a, _ := NewHTTPAdapter(server.URL, strings.Repeat("a", 32))
			err := a.Command(context.Background(), CommandRequest{})
			var refused *BridgeResponseError
			if !errors.As(err, &refused) || refused.Status != 409 || refused.Reason != test.reason {
				t.Fatalf("wrong bounded diagnostic: %v", err)
			}
			if strings.Contains(fmt.Sprint(err), "secret") || strings.Contains(fmt.Sprint(err), "private-token") {
				t.Fatal("untrusted response contents escaped")
			}
		})
	}
}

func TestHTTPRefusalBodyKeepsOriginalDeadline(t *testing.T) {
	requests := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"bridge_refused",`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	a, _ := NewHTTPAdapter(server.URL, strings.Repeat("a", 32))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := a.Command(ctx, CommandRequest{})
	var refused *BridgeResponseError
	if !errors.As(err, &refused) || refused.Status != 409 || refused.Reason != "" || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("diagnostic response read escaped original deadline: %v", err)
	}
	if len(requests) != 1 {
		t.Fatal("extra diagnostic request made")
	}
	if _, ok := a.LastCommandReceipt(); ok {
		t.Fatal("partial refusal invented ACK")
	}
}

func TestPythonRefusalAllowlistCompatibility(t *testing.T) {
	raw, err := os.ReadFile("../../sim/go2_bridge/__main__.py")
	if err != nil {
		t.Fatal(err)
	}
	group := regexp.MustCompile(`(?s)REFUSAL_REASONS = frozenset\(\{(.*?)\}\)`).FindSubmatch(raw)
	if len(group) != 2 {
		t.Fatal("bridge refusal allowlist unavailable")
	}
	codes := regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(group[1], -1)
	if len(codes) != 37 {
		t.Fatal("review changed bridge reason cardinality")
	}
	for _, code := range codes {
		if !knownBridgeRefusal(string(code[1])) {
			t.Fatalf("bridge reason is not covered: %s", code[1])
		}
	}
}

type receiptAdapter struct {
	*fakeAdapter
	receipt BridgeCommandReceipt
	change  func(*BridgeCommandReceipt)
}

func (a *receiptAdapter) Command(ctx context.Context, request CommandRequest) error {
	err := a.fakeAdapter.Command(ctx, request)
	a.receipt = BridgeCommandReceipt{Request: request, Acknowledgment: BridgeCommandAcknowledgment{Accepted: true, Sequence: request.Sequence, NotAfterUnixMS: request.NotAfterUnixMS, SourceTimestampNS: (request.NotAfterUnixMS - 450) * int64(time.Millisecond)}, RequestStartedUnixNS: 10, ResponseCompletedUnixNS: 20, RoundTripElapsedNS: 10}
	if a.change != nil {
		a.change(&a.receipt)
	}
	return err
}
func (a *receiptAdapter) LastCommandReceipt() (BridgeCommandReceipt, bool) { return a.receipt, true }

func TestControllerReceiptFullBindingAndOptionalAdapter(t *testing.T) {
	for _, change := range []struct {
		name string
		fn   func(*BridgeCommandReceipt)
	}{
		{"valid", nil},
		{"capture", func(r *BridgeCommandReceipt) { r.Request.CaptureSequence++ }},
		{"task", func(r *BridgeCommandReceipt) { r.Request.TaskID = "other" }},
		{"spec", func(r *BridgeCommandReceipt) { r.Request.SpecDigest = "other" }},
		{"epoch", func(r *BridgeCommandReceipt) { r.Request.ExecutorEpoch++ }},
		{"session", func(r *BridgeCommandReceipt) { r.Request.Session = "other" }},
		{"command", func(r *BridgeCommandReceipt) { r.Request.BodyVX++ }},
		{"sequence", func(r *BridgeCommandReceipt) { r.Acknowledgment.Sequence++ }},
		{"deadline", func(r *BridgeCommandReceipt) { r.Acknowledgment.NotAfterUnixMS++ }},
		{"late-source", func(r *BridgeCommandReceipt) {
			r.Acknowledgment.SourceTimestampNS = r.Request.NotAfterUnixMS * int64(time.Millisecond)
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			c, _, a, _ := fixture(t)
			c.adapter = &receiptAdapter{fakeAdapter: a, change: change.fn}
			if err := c.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			d, ok := c.CommandObservation()
			if !ok || d.Request != a.commands[0] {
				t.Fatal("attempt missing")
			}
			if change.fn == nil {
				if d.ReceiptState != "matched" || d.Receipt == nil {
					t.Fatal("matching receipt unavailable")
				}
			} else if d.ReceiptState != "mismatched" || d.Receipt != nil || c.lastAcknowledgedReceipt != nil {
				t.Fatal("different command's receipt retained")
			}
			if len(a.commands) != 1 || a.stops != 0 {
				t.Fatal("optional diagnostic changed successful control")
			}
		})
	}
	c, _, _, _ := fixture(t)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, ok := c.CommandObservation()
	if !ok || d.ReceiptState != "unavailable" || d.Receipt != nil {
		t.Fatal("legacy adapter fabricated receipt")
	}
}

func TestFaultRetainsPriorReceiptAndCurrentUnknownAttempt(t *testing.T) {
	c, _, a, _ := fixture(t)
	c.adapter = &receiptAdapter{fakeAdapter: a}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, _ := c.CommandObservation()
	a.commandErr = &BridgeResponseError{Status: 409, Reason: "publication_completed_late"}
	if _, err := c.Tick(context.Background()); err == nil {
		t.Fatal("failed command accepted")
	}
	shot, ok := c.FaultSnapshot()
	if !ok || shot.LastAcknowledgedReceipt == nil || shot.LastAcknowledgedReceipt.Request != first.Request || shot.LastCommandDiagnostic == nil || shot.LastCommandDiagnostic.Request.Sequence <= first.Request.Sequence || shot.LastCommandDiagnostic.Receipt != nil || shot.LastCommandDiagnostic.Outcome != "failed_or_unknown" || shot.LastCommandDiagnostic.Failure.Reason != "publication_completed_late" {
		t.Fatal("fault lost exact prior receipt/current uncertain outcome")
	}
	before, err := json.Marshal(shot)
	if err != nil {
		t.Fatal(err)
	}
	shot.LastAcknowledgedReceipt.Acknowledgment.SourceTimestampNS = 1
	shot.LastCommandDiagnostic.Request.Sequence = 999
	_, _ = c.Tick(context.Background())
	again, _ := c.FaultSnapshot()
	after, err := json.Marshal(again)
	if err != nil || string(before) != string(after) {
		t.Fatal("first fault changed after stop or caller mutation")
	}
}

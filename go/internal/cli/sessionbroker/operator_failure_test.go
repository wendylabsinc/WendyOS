//go:build !windows

package sessionbroker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func operatorDiagnosticsFixture(t *testing.T) (string, Spec) {
	t.Helper()
	directory := t.TempDir()
	old := configDir
	configDir = func() (string, error) { return directory, nil }
	t.Cleanup(func() { configDir = old })
	dir := filepath.Join(directory, "sessions")
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	return dir, Spec{Key: "operator-context:fixture", CertFingerprint: strings.Repeat("a", 64), DiagnosticGeneration: "11111111-1111-4111-8111-111111111111", Operator: &OperatorSpec{}}
}

func TestOperatorFailureRecordPrivateBoundedAndGenerationScoped(t *testing.T) {
	dir, spec := operatorDiagnosticsFixture(t)
	d := newOperatorDiagnostics(dir, spec)
	secret := "PRIVATE KEY and Authorization: Bearer do-not-retain"
	d.record("tcp", &certs.IssuerPathError{Err: errors.New(secret)}, false)
	d.record("", context.DeadlineExceeded, true)
	data, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(d.path)
	if len(data) > operatorFailureBytes || info.Mode().Perm() != 0600 || strings.Contains(string(data), secret) {
		t.Fatalf("diagnostic is not bounded/private/redacted: mode=%v bytes=%d", info.Mode(), len(data))
	}
	err = operatorStartupError(spec, context.DeadlineExceeded)
	var startup *operatorStartupFailure
	if !errors.As(err, &startup) || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "issuer chain") || strings.Contains(err.Error(), secret) {
		t.Fatalf("concrete safe carrier cause was lost: %v", err)
	}
	if retry, steps := OperatorRecovery(err); retry || len(steps) == 0 {
		t.Fatalf("verification rejection should offer correction, not blind retry: %v %v", retry, steps)
	}
	foreign := spec
	foreign.DiagnosticGeneration = "22222222-2222-4222-8222-222222222222"
	if errors.As(operatorStartupError(foreign, context.DeadlineExceeded), &startup) {
		t.Fatal("another launch consumed stale diagnostics")
	}
	foreign = spec
	foreign.CertFingerprint = strings.Repeat("b", 64)
	if errors.As(operatorStartupError(foreign, context.DeadlineExceeded), &startup) {
		t.Fatal("another credential consumed diagnostics")
	}
	// A new exact owner invalidates the prior generation before networking.
	newOperatorDiagnostics(dir, foreign)
	if _, err := os.Stat(d.path); !os.IsNotExist(err) {
		t.Fatalf("new owner did not clear old record: %v", err)
	}
}

func TestOperatorFailureRecordSingleFileAndUnknownPayloadIgnored(t *testing.T) {
	dir, spec := operatorDiagnosticsFixture(t)
	d := newOperatorDiagnostics(dir, spec)
	var writers sync.WaitGroup
	for range 64 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			d.record("untrusted transport bytes", errors.New("secret remote payload"), false)
		}()
	}
	writers.Wait()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("diagnostics accumulated files: %d %v", len(entries), err)
	}
	data, _ := os.ReadFile(d.path)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "untrusted") {
		t.Fatal("arbitrary remote content was persisted")
	}
	for _, mutate := range []func(*operatorFailure){
		func(f *operatorFailure) { f.Version++ },
		func(f *operatorFailure) { f.Kind = "future-kind" },
		func(f *operatorFailure) { f.Transport = "secret transport" },
		func(f *operatorFailure) { f.ExitReason = "secret exit reason" },
		func(f *operatorFailure) { f.ExitKind = "secret exit kind" },
		func(f *operatorFailure) { f.At = time.Now().Add(-3 * operatormesh.JoinSetupTimeout).UnixMilli() },
	} {
		failure := *d.last
		mutate(&failure)
		data, _ := json.Marshal(failure)
		if err := os.WriteFile(d.path, data, 0600); err != nil {
			t.Fatal(err)
		}
		var startup *operatorStartupFailure
		if errors.As(operatorStartupError(spec, context.DeadlineExceeded), &startup) {
			t.Fatalf("invalid/old record was consumed: %+v", failure)
		}
	}
}

func TestOperatorFailureChildStartupCauseSurvivesDetachedBoundary(t *testing.T) {
	directory := shortTempDir(t)
	oldDir, oldLoad, oldLaunch, oldOpen := configDir, loadConfig, launchOperatorBroker, openOperatorSession
	t.Cleanup(func() {
		configDir, loadConfig, launchOperatorBroker, openOperatorSession = oldDir, oldLoad, oldLaunch, oldOpen
	})
	configDir = func() (string, error) { return directory, nil }
	principal := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/diagnostics"
	uri, _ := url.Parse(principal)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	credential := config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	loadConfig = func() (*config.Config, error) {
		return &config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{credential}}}}, nil
	}
	recipe := OperatorSpec{Version: 1, Automatic: true, Principal: principal}
	spec, err := operatorSpec(recipe, credential)
	if err != nil {
		t.Fatal(err)
	}
	path := operatorRecipePath(directory, principal)
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(recipe)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	openOperatorSession = func(_ context.Context, options operatormesh.Options) (*operatormesh.Session, error) {
		if options.ReportCarrierFailure != nil {
			options.ReportCarrierFailure("ble", &certs.IssuerPathError{Err: errors.New("sensitive original error")})
		}
		return nil, context.DeadlineExceeded
	}
	launchOperatorBroker = func(spec Spec) error {
		data, _ := json.Marshal(spec)
		// The actual child entrypoint loses its return value at the detached
		// process boundary. Only its bounded private diagnostics can cross back.
		_ = Run(context.Background(), base64.RawURLEncoding.EncodeToString(data), time.Minute, nil)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	_, err = connectOrStartOperator(ctx, spec, &credential)
	if !strings.Contains(err.Error(), "issuer chain") || !strings.Contains(err.Error(), "ble") || strings.Contains(err.Error(), "sensitive original error") {
		t.Fatalf("detached cause is still hidden: %v", err)
	}
	dir, dirErr := brokerDir()
	if dirErr != nil {
		t.Fatal(dirErr)
	}
	data, readErr := os.ReadFile(operatorFailurePath(dir, spec))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var terminal operatorFailure
	if json.Unmarshal(data, &terminal) != nil || terminal.ExitReason != "startup_failed" || terminal.ExitKind != "timeout" || terminal.Kind != "issuer_chain" {
		t.Fatalf("Run dropped terminal reason or preceding concrete cause: %s", data)
	}
}

func TestOperatorFailureRecordRejectsPublicDirectoryAndOversizeInput(t *testing.T) {
	dir, spec := operatorDiagnosticsFixture(t)
	d := newOperatorDiagnostics(dir, spec)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	d.record("tcp", context.DeadlineExceeded, false)
	if _, err := os.Stat(d.path); !os.IsNotExist(err) {
		t.Fatalf("wrote diagnostics in nonprivate directory: %v", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.path, []byte(strings.Repeat("x", operatorFailureBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	var startup *operatorStartupFailure
	if errors.As(operatorStartupError(spec, context.DeadlineExceeded), &startup) {
		t.Fatal("oversize diagnostic consumed")
	}
}

type operatorFailurePinRejection struct{}

func (operatorFailurePinRejection) Error() string         { return "private pin error payload" }
func (operatorFailurePinRejection) BlockingPinRejection() {}

func TestOperatorFailureChangedVerificationCauseBypassesRepeatThrottle(t *testing.T) {
	for _, initial := range []struct {
		name string
		err  error
	}{{"timeout", context.DeadlineExceeded}, {"closed", io.EOF}} {
		for _, rejected := range []struct {
			name, kind string
			err        error
		}{
			{"issuer", "issuer_chain", &certs.IssuerPathError{Err: errors.New("private issuer payload")}},
			{"certificate", "certificate", x509.UnknownAuthorityError{}},
			{"pin", "device_pin", operatorFailurePinRejection{}},
		} {
			for _, transport := range []string{"tcp", "ble"} {
				t.Run(initial.name+"/"+rejected.name+"/"+transport, func(t *testing.T) {
					dir, spec := operatorDiagnosticsFixture(t)
					d := newOperatorDiagnostics(dir, spec)
					at := time.Now().Add(-time.Second)
					d.now = func() time.Time { return at }
					d.record("tcp", initial.err, false)
					at = at.Add(500 * time.Millisecond)
					d.record(transport, rejected.err, false)
					d.record("", context.DeadlineExceeded, true)
					if d.last.Kind != rejected.kind || d.last.Transport != transport || d.last.At != at.UnixMilli() {
						t.Fatalf("changed verification failure was lost: %+v", d.last)
					}
					startup := operatorStartupError(spec, context.DeadlineExceeded)
					retry, steps := OperatorRecovery(startup)
					if retry || len(steps) == 0 || strings.Contains(strings.Join(steps, " "), "then retry the connection") {
						t.Fatalf("verification failure received transient recovery: retry=%v steps=%v error=%v", retry, steps, startup)
					}
					if strings.Contains(startup.Error(), "private") {
						t.Fatalf("raw verification payload leaked: %v", startup)
					}
				})
			}
		}
	}
}

func TestOperatorFailureEquivalentFloodRemainsThrottled(t *testing.T) {
	dir, spec := operatorDiagnosticsFixture(t)
	d := newOperatorDiagnostics(dir, spec)
	at := time.Now().Add(-2 * time.Second)
	d.now = func() time.Time { return at }
	d.record("untrusted initial transport", errors.New("private first payload"), false)
	first, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := time.Unix(1000000, 0)
	if err := os.Chtimes(d.path, sentinel, sentinel); err != nil {
		t.Fatal(err)
	}
	at = at.Add(500 * time.Millisecond)
	var writers sync.WaitGroup
	for range 64 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			d.record("untrusted repeated transport", errors.New("private different payload"), false)
		}()
	}
	writers.Wait()
	data, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(d.path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(first) || !info.ModTime().Equal(sentinel) || info.Mode().Perm() != 0600 || len(data) > operatorFailureBytes || len(entries) != 1 {
		t.Fatalf("equivalent failure flood rewrote or expanded private record: mode=%v modified=%v bytes=%d files=%d", info.Mode(), info.ModTime(), len(data), len(entries))
	}
	at = at.Add(time.Second)
	d.record("", errors.New("another private payload"), false)
	if d.last.At != at.UnixMilli() {
		t.Fatal("equivalent record was never refreshed after throttle window")
	}
}

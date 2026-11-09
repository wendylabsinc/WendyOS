package sessionbroker

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

const operatorFailureBytes = 1024

// Keep only the latest fixed diagnostic code. Never persist stderr, certificates,
// dial addresses, remote messages, or request payloads in a retained helper.
type operatorFailure struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
	Generation  string `json:"generation"`
	Kind        string `json:"kind"`
	Transport   string `json:"transport,omitempty"`
	At          int64  `json:"atUnixMs"`
	ExitReason  string `json:"exitReason,omitempty"`
	ExitKind    string `json:"exitKind,omitempty"`
	ExitAt      int64  `json:"exitAtUnixMs,omitempty"`
}

type operatorDiagnostics struct {
	mu   sync.Mutex
	path string
	spec Spec
	last *operatorFailure
	now  func() time.Time
}

type operatorDiagnosticsKey struct{}

func operatorFailurePath(dir string, spec Spec) string {
	_, state := spec.paths(dir)
	return strings.TrimSuffix(state, ".json") + ".failure.json"
}

func newOperatorDiagnostics(dir string, spec Spec) *operatorDiagnostics {
	if spec.DiagnosticGeneration == "" {
		return nil
	}
	d := &operatorDiagnostics{path: operatorFailurePath(dir, spec), spec: spec, now: time.Now}
	// Only the context owner calls this, after acquiring its identity lock.
	if validatePrivateFile(d.path, false) == nil {
		_ = os.Remove(d.path)
	}
	return d
}

func operatorCarrierReporter(ctx context.Context) func(string, error) {
	if d, ok := ctx.Value(operatorDiagnosticsKey{}).(*operatorDiagnostics); ok {
		return func(transport string, err error) { d.record(transport, err, false) }
	}
	return nil
}

func (d *operatorDiagnostics) record(transport string, err error, terminal bool) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, carriers.ErrUnsupportedPeer) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if validatePrivateDir(filepath.Dir(d.path)) != nil {
		return
	}
	// A join deadline must not erase its preceding concrete carrier failure.
	if terminal && d.last != nil {
		return
	}
	now := d.now()
	switch transport {
	case carriers.TCP, carriers.BLE, carriers.QUIC:
	default:
		transport = ""
	}
	failure := &operatorFailure{Version: 1, Fingerprint: d.spec.CertFingerprint, Generation: d.spec.DiagnosticGeneration, Kind: operatorFailureKind(err), Transport: transport, At: now.UnixMilli()}
	// Suppress only equivalent repeats. A new certificate/pin failure must
	// replace a preceding timeout immediately so recovery advice stays accurate.
	if d.last != nil && failure.Kind == d.last.Kind && failure.Transport == d.last.Transport && now.Sub(time.UnixMilli(d.last.At)) < time.Second {
		return
	}
	data, marshalErr := json.Marshal(failure)
	if marshalErr != nil || len(data) > operatorFailureBytes {
		return
	}
	if statErr := validatePrivateFile(d.path, false); statErr != nil && !os.IsNotExist(statErr) {
		return
	}
	if atomicfile.Write(d.path, data, 0600) == nil {
		d.last = failure
	}
}

func operatorFailureKind(err error) string {
	var issuer *certs.IssuerPathError
	var pin certs.BlockingPinError
	var authority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var network net.Error
	switch {
	case errors.As(err, &issuer):
		return "issuer_chain"
	case errors.As(err, &pin):
		return "device_pin"
	case errors.As(err, &authority), errors.As(err, &invalid):
		return "certificate"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "connection_closed"
	default:
		return "attachment_failed"
	}
}

func (f operatorFailure) message() string {
	switch f.Kind {
	case "issuer_chain":
		return "the router certificate issuer chain could not be verified"
	case "device_pin":
		return "the peer does not match the saved device identity"
	case "certificate":
		return "the router certificate is not valid for this connection"
	case "timeout":
		return "the connection to a nearby router timed out"
	case "connection_closed":
		return "the connection to a nearby router closed"
	case "attachment_failed":
		return "the connection to a nearby router failed"
	case "broker_exit":
		return "the local Wendy Net background router stopped"
	}
	return ""
}

type operatorStartupFailure struct {
	cause   error
	failure operatorFailure
}

func (e *operatorStartupFailure) Error() string {
	phase := "connection"
	if e.failure.Transport != "" {
		phase = e.failure.Transport
	}
	return fmt.Sprintf("could not join Wendy Net from this computer; last %s connection attempt: %s: %v", phase, e.failure.message(), e.cause)
}
func (e *operatorStartupFailure) Unwrap() error { return e.cause }

func operatorStartupError(spec Spec, cause error) error {
	dir, err := brokerDir()
	if err != nil || validatePrivateDir(dir) != nil || spec.DiagnosticGeneration == "" {
		return fmt.Errorf("could not join Wendy Net from this computer: %w", cause)
	}
	path := operatorFailurePath(dir, spec)
	if validatePrivateFile(path, false) != nil {
		return fmt.Errorf("could not join Wendy Net from this computer: %w", cause)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not join Wendy Net from this computer: %w", cause)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, operatorFailureBytes+1))
	var failure operatorFailure
	now := time.Now()
	if err != nil || len(data) > operatorFailureBytes || json.Unmarshal(data, &failure) != nil || failure.Version != 1 || failure.Fingerprint != spec.CertFingerprint || failure.Generation != spec.DiagnosticGeneration || failure.message() == "" || (failure.ExitReason != "" && !validOperatorExitReason(failure.ExitReason)) || (failure.ExitKind != "" && !validOperatorExitKind(failure.ExitKind)) || failure.At > now.Add(time.Second).UnixMilli() || now.Sub(time.UnixMilli(failure.At)) > 2*operatormesh.JoinSetupTimeout {
		return fmt.Errorf("could not join Wendy Net from this computer: %w", cause)
	}
	switch failure.Transport {
	case "", carriers.TCP, carriers.BLE, carriers.QUIC:
	default:
		return fmt.Errorf("could not join Wendy Net from this computer: %w", cause)
	}
	return &operatorStartupFailure{cause: cause, failure: failure}
}

// recordExit runs only after runOperator has joined its producers. A generic
// shutdown annotates, rather than erases, the latest concrete carrier failure.
func (d *operatorDiagnostics) recordExit(cause *operatorExitCause) {
	if cause == nil || !validOperatorExitReason(cause.reason) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if validatePrivateDir(filepath.Dir(d.path)) != nil {
		return
	}
	now := d.now().UnixMilli()
	failure := operatorFailure{Version: 1, Fingerprint: d.spec.CertFingerprint, Generation: d.spec.DiagnosticGeneration, Kind: "broker_exit", At: now}
	if d.last != nil {
		failure = *d.last
	} else if cause.err != nil && !errors.Is(cause.err, context.Canceled) {
		// Initial explicit attachment errors precede any carrier callback.
		// Keep their existing explanation and recovery classification.
		failure.Kind = operatorFailureKind(cause.err)
	}
	failure.ExitAt = now
	failure.ExitReason = cause.reason
	failure.ExitKind = ""
	if cause.err != nil {
		failure.ExitKind = operatorFailureKind(cause.err)
		if errors.Is(cause.err, context.Canceled) {
			failure.ExitKind = "canceled"
		}
	}
	data, err := json.Marshal(failure)
	if err != nil || len(data) > operatorFailureBytes {
		return
	}
	if err = validatePrivateFile(d.path, false); err != nil && !os.IsNotExist(err) {
		return
	}
	if atomicfile.Write(d.path, data, 0600) == nil {
		d.last = &failure
	}
}

func validOperatorExitKind(kind string) bool {
	switch kind {
	case "issuer_chain", "device_pin", "certificate", "timeout", "connection_closed", "attachment_failed", "canceled":
		return true
	}
	return false
}

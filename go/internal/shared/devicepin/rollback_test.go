package devicepin_test

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
)

func TestStore_RollbackLeavesRecordUnchanged(t *testing.T) {
	const legacy = "urn:wendy:org:7:asset:42"
	for _, migrate := range []bool{false, true} {
		t.Run(fmt.Sprintf("migrate=%v", migrate), func(t *testing.T) {
			dir := t.TempDir()
			store, err := devicepin.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			first := assetCert(t, 7, "42", time.Now().Add(time.Hour))
			if err := store.CheckAndUpdate(first, "box"); err != nil {
				t.Fatal(err)
			}
			other := assetCert(t, 7, "42", first.NotAfter)
			other.NotBefore = first.NotBefore.Add(-time.Hour)
			if migrate {
				principal, _ := url.Parse(renamePrincipal)
				other.URIs = append(other.URIs, principal)
			}
			path := filepath.Join(dir, "known_devices.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Reopen to prove the issuance threshold survives persistence.
			store, err = devicepin.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			var mismatch *devicepin.PinMismatchError
			for range 2 {
				if err := store.CheckAndUpdate(other, "changed name"); !errors.As(err, &mismatch) {
					t.Fatalf("rollback error = %v, want PinMismatchError", err)
				}
				if mismatch.Key != legacy {
					t.Fatalf("recovery key = %q, want stored key %q", mismatch.Key, legacy)
				}
				if !store.Has(legacy) || store.Has(renamePrincipal) {
					t.Fatal("rejection changed the in-memory identity keys")
				}
			}
			// Replaying the accepted certificate leaves the persisted record intact.
			if err := store.CheckAndUpdate(first, "box"); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejection changed persisted record: %s, %v", after, err)
			}
			if err := store.Remove(mismatch.Key); err != nil {
				t.Fatal(err)
			}
			if err := store.CheckAndUpdate(other, "box"); err != nil {
				t.Fatalf("explicit unpin must allow recovery: %v", err)
			}
		})
	}
}

func TestStore_SameKeyCannotLowerIssuanceTime(t *testing.T) {
	dir := t.TempDir()
	store, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := assetCert(t, 7, "42", time.Now().Add(time.Hour))
	if err := store.CheckAndUpdate(first, "box"); err != nil {
		t.Fatal(err)
	}
	older := *first
	older.NotBefore = first.NotBefore.Add(-2 * time.Hour)
	if err := store.CheckAndUpdate(&older, "box"); err != nil {
		t.Fatal(err)
	}
	store, err = devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	other := assetCert(t, 7, "42", first.NotAfter)
	other.NotBefore = first.NotBefore.Add(-time.Hour)
	var mismatch *devicepin.PinMismatchError
	if err := store.CheckAndUpdate(other, "box"); !errors.As(err, &mismatch) {
		t.Fatalf("older same-key replay lowered the threshold: %v", err)
	}
	// Equal issuance times are permitted, including renewals within one second.
	other.NotBefore = first.NotBefore
	if err := store.CheckAndUpdate(other, "box"); err != nil {
		t.Fatalf("equal NotBefore must allow rotation: %v", err)
	}
}

func TestStore_LegacyRecordLearnsIssuanceTime(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(fmt.Sprintf("rotate=%v", rotate), func(t *testing.T) {
			dir := t.TempDir()
			first := assetCert(t, 7, "42", time.Now().Add(time.Hour))
			fingerprint := fmt.Sprintf("sha256:%x", sha256.Sum256(first.RawSubjectPublicKeyInfo))
			path := filepath.Join(dir, "known_devices.json")
			data := fmt.Sprintf(`{"urn:wendy:org:7:asset:42":{"spkiFingerprint":%q,"displayName":"box","notAfter":%q}}`, fingerprint, first.NotAfter.Format(time.RFC3339))
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := devicepin.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if rotate {
				first = assetCert(t, 7, "42", first.NotAfter)
			}
			if err := store.CheckAndUpdate(first, "box"); err != nil {
				t.Fatal(err)
			}
			dataBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var records map[string]devicepin.PinnedDevice
			if err := json.Unmarshal(dataBytes, &records); err != nil {
				t.Fatal(err)
			}
			if got := records["urn:wendy:org:7:asset:42"].NotBefore; got != first.NotBefore.Format(time.RFC3339) {
				t.Fatalf("learned NotBefore = %q, want %v", got, first.NotBefore)
			}
		})
	}
}

func TestStore_LogsAcceptedKeyChanges(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	store, err := devicepin.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := assetCert(t, 7, "42", time.Now().Add(time.Hour))
	renewed := assetCert(t, 7, "42", first.NotAfter)
	renewed.NotBefore = first.NotBefore.Add(time.Minute)
	for _, leaf := range []*x509.Certificate{first, first, renewed, renewed} {
		if err := store.CheckAndUpdate(leaf, "box"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CheckAndUpdate(first, "box"); err == nil {
		t.Fatal("accepted rollback")
	}
	if strings.Count(output.String(), "\n") != 1 {
		t.Fatalf("expected exactly one rotation log: %s", &output)
	}
	var entry map[string]string
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["level"] != "INFO" || entry["identity"] != "urn:wendy:org:7:asset:42" ||
		entry["old_fingerprint"] != fmt.Sprintf("sha256:%x", sha256.Sum256(first.RawSubjectPublicKeyInfo)) ||
		entry["new_fingerprint"] != fmt.Sprintf("sha256:%x", sha256.Sum256(renewed.RawSubjectPublicKeyInfo)) {
		t.Fatalf("incomplete rotation log: %s", &output)
	}
}

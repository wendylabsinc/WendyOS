//go:build windows

package winusb

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func disposableSigningKey(t *testing.T) string {
	t.Helper()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("WendyUSBTest-%x", id)
	if err := ensureSigningKeyIn(name, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		container, _ := windows.UTF16PtrFromString(name)
		provider, _ := windows.UTF16PtrFromString(msEnhRSAAESProv)
		var ignored windows.Handle
		if err := windows.CryptAcquireContext(&ignored, container, provider, provRSAAES, windows.CRYPT_DELETEKEYSET); err != nil {
			t.Errorf("deleting disposable signing key: %v", err)
		}
	})
	return name
}

func memoryCertStore(t *testing.T) windows.Handle {
	t.Helper()
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_MEMORY, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CertCloseStore(store, 0) })
	return store
}

func certDER(ctx *windows.CertContext) []byte {
	return bytes.Clone(unsafe.Slice(ctx.EncodedCert, ctx.Length))
}

func TestSigningCertReuseAcrossDriverFamilies(t *testing.T) {
	key := disposableSigningKey(t)
	store := memoryCertStore(t)
	cert, err := newSigningCert(key, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cert.Free()
	original := certDER(cert.ctx)
	if err := windows.CertAddCertificateContextToStore(store, cert.ctx, certStoreAddReplaceExisting, nil); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []DriverProfile{JetsonDriver(), DragonwingDriver(), JetsonDriver()} {
		reused, err := findSigningCertInStore(store, key, false, time.Now())
		if err != nil || reused == nil {
			t.Fatalf("%s did not reuse certificate: %v", profile.Name, err)
		}
		if !bytes.Equal(original, certDER(reused.ctx)) {
			t.Fatal("minted a different certificate")
		}
		// Actually sign each family's catalog with the reused Windows key.
		// Nothing is staged or added to a system certificate store.
		dir := t.TempDir()
		inf := filepath.Join(dir, profile.inf)
		if err := os.WriteFile(inf, []byte(generateProfileINF(profile)), 0600); err != nil {
			t.Fatal(err)
		}
		err = buildAndSignCatalog(filepath.Join(dir, profile.catalog), inf, profile.hardwareIDs(), reused)
		if err != nil {
			reused.Free()
			t.Fatal(err)
		}
		if err := windows.CertAddCertificateContextToStore(store, reused.ctx, certStoreAddReplaceExisting, nil); err != nil {
			reused.Free()
			t.Fatal(err)
		}
		reused.Free()
	}
	first, err := windows.CertEnumCertificatesInStore(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := windows.CertEnumCertificatesInStore(store, first)
	if second != nil {
		windows.CertFreeCertificateContext(second)
		t.Fatal("reuse accumulated certificates")
	}
	// Simulate expiry without changing the host clock. The old certificate
	// must remain in the store for packages that were previously signed by it.
	reused, err := findSigningCertInStore(store, key, false, time.Now().AddDate(2, 0, 0))
	if err != nil || reused != nil {
		reused.Free()
		t.Fatalf("expired certificate reused: %v", err)
	}
	retained, err := windows.CertEnumCertificatesInStore(store, nil)
	if err != nil {
		t.Fatal("expired package signer was deleted")
	}
	windows.CertFreeCertificateContext(retained)
}

func TestSigningCertRejectsWrongKeyAndScope(t *testing.T) {
	keyA, keyB := disposableSigningKey(t), disposableSigningKey(t)
	cert, err := newSigningCert(keyA, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cert.Free()
	if !signingKeyMatches(cert.ctx, keyA, false) {
		t.Fatal("own signing key rejected")
	}
	if signingKeyMatches(cert.ctx, keyA, true) || signingKeyMatches(cert.ctx, keyB, false) {
		t.Fatal("wrong key container or scope accepted")
	}
	// Metadata alone must not establish ownership: point A's certificate at
	// another real key container and require the public-key comparison to fail.
	container, _ := windows.UTF16PtrFromString(keyB)
	provider, _ := windows.UTF16PtrFromString(msEnhRSAAESProv)
	info := cryptKeyProvInfo{pwszContainerName: container, pwszProvName: provider, dwProvType: provRSAAES, dwKeySpec: atSignature}
	if r, _, err := procCertSetCertificateContextProperty.Call(uintptr(unsafe.Pointer(cert.ctx)), certKeyProvInfoPropID, 0, uintptr(unsafe.Pointer(&info))); r == 0 {
		t.Fatal(err)
	}
	if signingKeyMatches(cert.ctx, keyB, false) {
		t.Fatal("certificate accepted with mismatched private key")
	}
}

func TestUsableSigningCertificate(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, tc := range []struct {
		name   string
		change func(*x509.Certificate)
		want   bool
	}{
		{"valid", func(*x509.Certificate) {}, true},
		{"expired", func(c *x509.Certificate) { c.NotAfter = now.Add(-time.Minute) }, false},
		{"not yet valid", func(c *x509.Certificate) { c.NotBefore = now.Add(time.Minute) }, false},
		{"wrong subject", func(c *x509.Certificate) { c.Subject.CommonName = "Unrelated signer" }, false},
		{"TLS usage", func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} }, false},
		{"unrestricted usage", func(c *x509.Certificate) { c.ExtKeyUsage = nil }, false},
		{"additional usage", func(c *x509.Certificate) { c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageServerAuth) }, false},
		{"CA", func(c *x509.Certificate) { c.IsCA = true }, false},
		{"wrong key usage", func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageKeyEncipherment }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: strings.TrimPrefix(certSubject, "CN=")},
				NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true,
				SignatureAlgorithm: x509.SHA256WithRSA, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
			tc.change(tmpl)
			der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			if got := usableSigningCertificate(cert, now); got != tc.want {
				t.Fatalf("usable=%v want=%v", got, tc.want)
			}
			if tc.want {
				cert.Signature[0] ^= 1
				if usableSigningCertificate(cert, now) {
					t.Fatal("invalid self-signature accepted")
				}
			}
		})
	}
}

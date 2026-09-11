//go:build windows

package winusb

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Look in both stores: a previous interrupted install may have trusted the
// certificate in only one. installToStores repairs the missing entry later.
func findStoredSigningCert(machine bool) (*signingCert, error) {
	scope := uint32(windows.CERT_SYSTEM_STORE_CURRENT_USER)
	if machine {
		scope = windows.CERT_SYSTEM_STORE_LOCAL_MACHINE
	}
	for _, name := range []string{"TrustedPublisher", "Root"} {
		wname, _ := windows.UTF16PtrFromString(name)
		store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
			scope|windows.CERT_STORE_OPEN_EXISTING_FLAG|windows.CERT_STORE_READONLY_FLAG, uintptr(unsafe.Pointer(wname)))
		if err != nil {
			return nil, fmt.Errorf("opening signing certificate store %s: %w", name, err)
		}
		cert, err := findSigningCertInStore(store, keyContainerName, machine, time.Now())
		windows.CertCloseStore(store, 0)
		if err != nil || cert != nil {
			return cert, err
		}
	}
	return nil, nil
}

// Prefer the newest usable certificate when upgrading a host that already has
// several. Do not delete older entries: staged catalogs can still require them.
func findSigningCertInStore(store windows.Handle, keyName string, machine bool, now time.Time) (*signingCert, error) {
	var chosen *signingCert
	var newest time.Time
	var prev *windows.CertContext
	for {
		ctx, err := windows.CertEnumCertificatesInStore(store, prev)
		// Enumeration frees prev, including on its final call.
		if err != nil {
			if errors.Is(err, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return chosen, nil
			}
			chosen.Free()
			return nil, fmt.Errorf("enumerating signing certificates: %w", err)
		}
		prev = ctx
		cert, err := x509.ParseCertificate(unsafe.Slice(ctx.EncodedCert, ctx.Length))
		if err != nil || !usableSigningCertificate(cert, now) || !cert.NotBefore.After(newest) {
			continue
		}
		if !signingKeyMatches(ctx, keyName, machine) {
			continue
		}
		dup := windows.CertDuplicateCertificateContext(ctx)
		if dup == nil {
			windows.CertFreeCertificateContext(ctx)
			chosen.Free()
			return nil, fmt.Errorf("duplicating signing certificate")
		}
		chosen.Free()
		chosen = &signingCert{ctx: dup}
		newest = cert.NotBefore
	}
}

func usableSigningCertificate(cert *x509.Certificate, now time.Time) bool {
	if cert.Subject.CommonName != strings.TrimPrefix(certSubject, "CN=") ||
		!bytes.Equal(cert.RawSubject, cert.RawIssuer) || now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) ||
		cert.IsCA || cert.SignatureAlgorithm != x509.SHA256WithRSA ||
		len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageCodeSigning ||
		len(cert.UnknownExtKeyUsage) != 0 || len(cert.UnhandledCriticalExtensions) != 0 {
		return false
	}
	if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return false
	}
	return cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) == nil
}

// The subject name alone is not ownership. Require the expected machine/user
// key container and provider, then have CryptoAPI compare the actual public key
// and acquire its private key without caching, UI, or property repair.
func signingKeyMatches(ctx *windows.CertContext, keyName string, machine bool) bool {
	getProperty := modcrypt32.NewProc("CertGetCertificateContextProperty")
	var size uint32
	if r, _, _ := getProperty.Call(uintptr(unsafe.Pointer(ctx)), certKeyProvInfoPropID, 0, uintptr(unsafe.Pointer(&size))); r == 0 {
		return false
	}
	if size < uint32(unsafe.Sizeof(cryptKeyProvInfo{})) {
		return false
	}
	buf := make([]byte, size)
	if r, _, _ := getProperty.Call(uintptr(unsafe.Pointer(ctx)), certKeyProvInfoPropID, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return false
	}
	info := (*cryptKeyProvInfo)(unsafe.Pointer(&buf[0]))
	flags := uint32(0)
	if machine {
		flags = cryptMachineKeyset
	}
	matches := windows.UTF16PtrToString(info.pwszContainerName) == keyName &&
		windows.UTF16PtrToString(info.pwszProvName) == msEnhRSAAESProv &&
		info.dwProvType == provRSAAES && info.dwKeySpec == atSignature && info.dwFlags == flags && info.cProvParam == 0
	runtime.KeepAlive(buf)
	if !matches {
		return false
	}
	var key windows.Handle
	var spec uint32
	var free bool
	err := windows.CryptAcquireCertificatePrivateKey(ctx,
		windows.CRYPT_ACQUIRE_COMPARE_KEY_FLAG|windows.CRYPT_ACQUIRE_NO_HEALING|windows.CRYPT_ACQUIRE_SILENT_FLAG,
		nil, &key, &spec, &free)
	if err != nil {
		return false
	}
	if free {
		windows.CryptReleaseContext(key, 0)
	}
	return spec == atSignature
}

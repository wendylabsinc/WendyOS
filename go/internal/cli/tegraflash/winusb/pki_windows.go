//go:build windows

package winusb

// Runtime self-signed code-signing certificate, used to sign the driver
// catalog. This mirrors the approach Zadig/libwdi use on millions of machines:
// a keypair + self-signed cert are generated locally, the catalog is signed with
// it, and the (public) certificate is placed in the machine's Trusted Root and
// Trusted Publisher stores so Windows accepts the package. The private key never
// leaves this machine. Previously staged packages retain their signing trust.
//
// This is the v1 (early-access) trust mechanism. The v2 endgame replaces it with
// a Microsoft attestation-signed package (no local cert, no Root-store write).

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// keyContainerName is the machine key container holding the signing key. Fixed so
// a re-run reuses it rather than littering new containers.
const keyContainerName = "WendyLabsUSBFlashing"

// certSubject / certFriendlyName identify the cert in the store for the user, so
// a curious admin can see exactly what wendy added (and remove it if they wish).
const (
	certSubject      = "CN=Wendy Labs (USB flashing driver signing)"
	certFriendlyName = "Wendy Labs (USB flashing driver signing)"

	msEnhRSAAESProv = "Microsoft Enhanced RSA and AES Cryptographic Provider"
)

// signingCert is a self-signed cert whose private key lives in a machine key
// container, suitable for passing to SignerSignEx (via the cert's linked
// CERT_KEY_PROV_INFO) and for adding to the trust stores.
type signingCert struct {
	ctx *windows.CertContext
}

// createSigningCert reuses a valid certificate for our signing key, creating
// one only when none is reusable. machineKeyset selects the machine key
// container (needed for the elevated install path so the staged package validates
// system-wide); a user keyset lets the non-elevated debug path sign+inspect.
// The caller must Free the result.
func createSigningCert(machineKeyset bool) (*signingCert, error) {
	if err := ensureSigningKey(machineKeyset); err != nil {
		return nil, err
	}
	cert, err := findStoredSigningCert(machineKeyset)
	if err != nil || cert != nil {
		return cert, err
	}
	return newSigningCert(keyContainerName, machineKeyset)
}

// newSigningCert requires an existing signing key. A separate constructor lets
// tests use disposable user keys and memory stores without changing host trust.
func newSigningCert(keyName string, machineKeyset bool) (*signingCert, error) {
	keysetFlag := uint32(0)
	if machineKeyset {
		keysetFlag = cryptMachineKeyset
	}

	// 2. Encode the subject name "CN=..." into a CERT_NAME_BLOB.
	nameBlob, err := certStrToName(certSubject)
	if err != nil {
		return nil, err
	}

	// 3. Describe the key container so the cert carries CERT_KEY_PROV_INFO and
	//    SignerSignEx can locate the private key.
	container, _ := windows.UTF16PtrFromString(keyName)
	provName, _ := windows.UTF16PtrFromString(msEnhRSAAESProv)
	keyProv := cryptKeyProvInfo{
		pwszContainerName: container,
		pwszProvName:      provName,
		dwProvType:        provRSAAES,
		dwFlags:           keysetFlag,
		dwKeySpec:         atSignature,
	}

	// 4. Sign the cert with SHA-256/RSA.
	sigOID, _ := windows.BytePtrFromString(oidRSASHA256RSA)
	sigAlgo := cryptAlgorithmIdentifier{pszObjId: sigOID}

	// 4b. Add the code-signing EKU extension. Driver-package verification
	//     (WinVerifyTrust DRIVER_ACTION_VERIFY) requires the signing cert to
	//     carry szOID_PKIX_KP_CODE_SIGNING, or it reports TRUST_E_NOSIGNATURE.
	exts, extErr := codeSigningExtensions()
	if extErr != nil {
		return nil, extErr
	}

	// 5. Create the self-signed certificate. hCryptProvOrNCryptKey = 0 makes it
	//    acquire the key via pKeyProvInfo. Start/end time NULL => now .. now+1yr.
	r, _, e := procCertCreateSelfSignCertificate.Call(
		0,
		uintptr(unsafe.Pointer(&nameBlob)),
		0,
		uintptr(unsafe.Pointer(&keyProv)),
		uintptr(unsafe.Pointer(&sigAlgo)),
		0, 0,
		uintptr(unsafe.Pointer(exts)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CertCreateSelfSignCertificate: %w", e)
	}
	ctx := (*windows.CertContext)(unsafe.Pointer(r))

	// 6. Set a friendly name so the cert is identifiable in certmgr.
	if err := setFriendlyName(ctx, certFriendlyName); err != nil {
		windows.CertFreeCertificateContext(ctx)
		return nil, err
	}
	return &signingCert{ctx: ctx}, nil
}

// Free releases the certificate context.
func (c *signingCert) Free() {
	if c != nil && c.ctx != nil {
		windows.CertFreeCertificateContext(c.ctx)
		c.ctx = nil
	}
}

// ensureSigningKey creates the key container and an AT_SIGNATURE RSA-2048 key.
// If the container already exists it is reused (opened, key left in place).
// machineKeyset selects the machine vs user key store.
func ensureSigningKey(machineKeyset bool) error {
	return ensureSigningKeyIn(keyContainerName, machineKeyset)
}

func ensureSigningKeyIn(keyName string, machineKeyset bool) error {
	container, _ := windows.UTF16PtrFromString(keyName)
	provName, _ := windows.UTF16PtrFromString(msEnhRSAAESProv)
	keysetFlag := uint32(0)
	if machineKeyset {
		keysetFlag = cryptMachineKeyset
	}

	var hProv windows.Handle
	// Try to create a fresh keyset.
	err := windows.CryptAcquireContext(&hProv, container, provName, provRSAAES, cryptNewKeyset|keysetFlag)
	if err != nil {
		// Already exists: open it (no NEWKEYSET) and reuse its key.
		if err2 := windows.CryptAcquireContext(&hProv, container, provName, provRSAAES, keysetFlag); err2 != nil {
			return fmt.Errorf("CryptAcquireContext (create: %v; open: %w)", err, err2)
		}
		windows.CryptReleaseContext(hProv, 0)
		return nil
	}
	defer windows.CryptReleaseContext(hProv, 0)

	// Generate a 2048-bit AT_SIGNATURE keypair, exportable. Key length is the high
	// word of dwFlags.
	const cryptExportable = 0x00000001
	dwFlags := uint32(cryptExportable) | (uint32(2048) << 16)
	var hKey windows.Handle
	if err := cryptGenKey(hProv, atSignature, dwFlags, &hKey); err != nil {
		return fmt.Errorf("CryptGenKey: %w", err)
	}
	cryptDestroyKey(hKey)
	return nil
}

// installToStores adds the certificate to the machine Trusted Root and Trusted
// Publisher stores, so Windows trusts the catalog we signed with it.
func (c *signingCert) installToStores() error {
	for _, store := range []string{"Root", "TrustedPublisher"} {
		if err := addCertToSystemStore(c.ctx, store); err != nil {
			return fmt.Errorf("adding cert to %s store: %w", store, err)
		}
	}
	return nil
}

// addCertToSystemStore opens a LocalMachine system store by name and adds ctx.
func addCertToSystemStore(ctx *windows.CertContext, storeName string) error {
	name, _ := windows.UTF16PtrFromString(storeName)
	store, err := windows.CertOpenStore(
		windows.CERT_STORE_PROV_SYSTEM,
		0,
		0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE,
		uintptr(unsafe.Pointer(name)),
	)
	if err != nil {
		return fmt.Errorf("CertOpenStore(%s): %w", storeName, err)
	}
	defer windows.CertCloseStore(store, 0)

	// Keep prior signing certificates. A different staged package may still
	// rely on one when its board is connected later; installing Qualcomm must
	// not invalidate the trust of an already-staged Jetson package (or vice versa).
	// Normal installs reuse the same certificate, so this replaces its existing
	// entry. Only renewal adds a certificate; old package signers are retained.

	if err := windows.CertAddCertificateContextToStore(store, ctx, certStoreAddReplaceExisting, nil); err != nil {
		return fmt.Errorf("CertAddCertificateContextToStore(%s): %w", storeName, err)
	}
	return nil
}

// codeSigningExtensions builds a CERT_EXTENSIONS containing a single non-critical
// Enhanced Key Usage extension listing szOID_PKIX_KP_CODE_SIGNING. The returned
// pointer references Go-owned backing memory that must stay alive until
// CertCreateSelfSignCertificate returns (it copies the data), which it does since
// the caller uses it synchronously.
func codeSigningExtensions() (*certExtensions, error) {
	// CERT_ENHKEY_USAGE with one OID pointer.
	ekuOID, err := windows.BytePtrFromString(oidCodeSigningEKU)
	if err != nil {
		return nil, err
	}
	oidList := []*byte{ekuOID}
	eku := certEnhKeyUsage{
		cUsageIdentifier:     1,
		rgpszUsageIdentifier: &oidList[0],
	}
	// Encode the EKU to DER.
	var cb uint32
	if r, _, e := procCryptEncodeObject.Call(
		x509ASNEncoding,
		x509EnhancedKeyUsage,
		uintptr(unsafe.Pointer(&eku)),
		0,
		uintptr(unsafe.Pointer(&cb)),
	); r == 0 {
		return nil, fmt.Errorf("CryptEncodeObject(EKU size): %w", e)
	}
	encoded := make([]byte, cb)
	if r, _, e := procCryptEncodeObject.Call(
		x509ASNEncoding,
		x509EnhancedKeyUsage,
		uintptr(unsafe.Pointer(&eku)),
		uintptr(unsafe.Pointer(&encoded[0])),
		uintptr(unsafe.Pointer(&cb)),
	); r == 0 {
		return nil, fmt.Errorf("CryptEncodeObject(EKU): %w", e)
	}

	extOID, _ := windows.BytePtrFromString(oidEnhancedKeyUsage)
	ext := &certExtension{
		pszObjId:  extOID,
		fCritical: 0,
		Value:     cryptoAPIBlob{cbData: cb, pbData: &encoded[0]},
	}
	// Keep backing slices alive by stashing them on the returned wrapper.
	exts := &certExtensions{cExtension: 1, rgExtension: ext}
	extKeepAlive = append(extKeepAlive, encoded, []byte{}) // retain encoded
	extPtrKeepAlive = append(extPtrKeepAlive, ekuOID, extOID, ext, &oidList[0])
	return exts, nil
}

// extKeepAlive / extPtrKeepAlive retain backing allocations referenced by the
// CERT_EXTENSIONS returned above until the cert is created (process-lifetime is
// fine; this runs once). Prevents the GC from freeing buffers the C call reads.
var (
	extKeepAlive    [][]byte
	extPtrKeepAlive []interface{}
)

// certStrToName encodes an X.500 name string ("CN=...") into a CERT_NAME_BLOB.
func certStrToName(name string) (cryptoAPIBlob, error) {
	wname, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return cryptoAPIBlob{}, err
	}
	// First call with nil buffer to size it.
	var cb uint32
	r, _, e := procCertStrToNameW.Call(
		x509ASNEncoding,
		uintptr(unsafe.Pointer(wname)),
		certX500NameStr,
		0, 0,
		uintptr(unsafe.Pointer(&cb)),
		0,
	)
	if r == 0 {
		return cryptoAPIBlob{}, fmt.Errorf("CertStrToNameW (size): %w", e)
	}
	buf := make([]byte, cb)
	r, _, e = procCertStrToNameW.Call(
		x509ASNEncoding,
		uintptr(unsafe.Pointer(wname)),
		certX500NameStr,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&cb)),
		0,
	)
	if r == 0 {
		return cryptoAPIBlob{}, fmt.Errorf("CertStrToNameW: %w", e)
	}
	return cryptoAPIBlob{cbData: cb, pbData: &buf[0]}, nil
}

// setFriendlyName sets CERT_FRIENDLY_NAME_PROP_ID on the cert context.
func setFriendlyName(ctx *windows.CertContext, name string) error {
	wname, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	blob := cryptoAPIBlob{
		cbData: uint32(len(wname) * 2),
		pbData: (*byte)(unsafe.Pointer(&wname[0])),
	}
	r, _, e := procCertSetCertificateContextProperty.Call(
		uintptr(unsafe.Pointer(ctx)),
		certFriendlyNamePropID,
		0,
		uintptr(unsafe.Pointer(&blob)),
	)
	if r == 0 {
		return fmt.Errorf("CertSetCertificateContextProperty(friendly name): %w", e)
	}
	return nil
}

// cryptGenKey wraps advapi32!CryptGenKey.
func cryptGenKey(hProv windows.Handle, algid uint32, dwFlags uint32, hKey *windows.Handle) error {
	proc := modadvapi32.NewProc("CryptGenKey")
	r, _, e := proc.Call(
		uintptr(hProv),
		uintptr(algid),
		uintptr(dwFlags),
		uintptr(unsafe.Pointer(hKey)),
	)
	if r == 0 {
		return e
	}
	return nil
}

// cryptDestroyKey wraps advapi32!CryptDestroyKey (best-effort).
func cryptDestroyKey(hKey windows.Handle) {
	proc := modadvapi32.NewProc("CryptDestroyKey")
	proc.Call(uintptr(hKey))
}

var modadvapi32 = windows.NewLazySystemDLL("advapi32.dll")

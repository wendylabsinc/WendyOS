//go:build windows

package winusb

// Win32 bindings not provided by golang.org/x/sys/windows, used by the driver
// installer (pki/catalog/driverinstall) and the WinUSB transport. Everything
// here resolves to DLLs present in a base Windows 11 install — crypt32,
// wintrust, mssign32, setupapi, newdev, cfgmgr32, winusb — so wendy needs no
// SDK tools, no shipped executables, and no cgo.
//
// Struct layouts follow the Windows SDK headers / MS Learn documentation for the
// LLP64 model (amd64 and arm64): DWORD/LONG/ALG_ID are 4 bytes, pointers and
// HANDLEs are 8 bytes; Go's natural alignment reproduces the C padding.

import (
	"golang.org/x/sys/windows"
)

var (
	modcrypt32  = windows.NewLazySystemDLL("crypt32.dll")
	modwintrust = windows.NewLazySystemDLL("wintrust.dll")
	modmssign32 = windows.NewLazySystemDLL("mssign32.dll")
	modsetupapi = windows.NewLazySystemDLL("setupapi.dll")
	modnewdev   = windows.NewLazySystemDLL("newdev.dll")

	// crypt32
	procCertCreateSelfSignCertificate     = modcrypt32.NewProc("CertCreateSelfSignCertificate")
	procCertStrToNameW                    = modcrypt32.NewProc("CertStrToNameW")
	procCertSetCertificateContextProperty = modcrypt32.NewProc("CertSetCertificateContextProperty")
	procCryptEncodeObject                 = modcrypt32.NewProc("CryptEncodeObject")

	// wintrust — catalog build
	procCryptCATOpen           = modwintrust.NewProc("CryptCATOpen")
	procCryptCATClose          = modwintrust.NewProc("CryptCATClose")
	procCryptCATPutCatAttrInfo = modwintrust.NewProc("CryptCATPutCatAttrInfo")
	procCryptCATPutMemberInfo  = modwintrust.NewProc("CryptCATPutMemberInfo")
	procCryptCATPutAttrInfo    = modwintrust.NewProc("CryptCATPutAttrInfo")
	procCryptCATPersistStore   = modwintrust.NewProc("CryptCATPersistStore")

	// wintrust — file hashing (SHA-256 catalogs)
	procCryptCATAdminAcquireContext2         = modwintrust.NewProc("CryptCATAdminAcquireContext2")
	procCryptCATAdminReleaseContext          = modwintrust.NewProc("CryptCATAdminReleaseContext")
	procCryptCATAdminCalcHashFromFileHandle2 = modwintrust.NewProc("CryptCATAdminCalcHashFromFileHandle2")

	// crypt32 — SIP (indirect data for catalog members)
	procCryptSIPRetrieveSubjectGuid = modcrypt32.NewProc("CryptSIPRetrieveSubjectGuid")
	procCryptSIPLoad                = modcrypt32.NewProc("CryptSIPLoad")

	// mssign32 — Authenticode signing of the catalog
	procSignerSignEx            = modmssign32.NewProc("SignerSignEx")
	procSignerFreeSignerContext = modmssign32.NewProc("SignerFreeSignerContext")

	// setupapi / newdev — stage + bind the driver package
	procSetupCopyOEMInfW                   = modsetupapi.NewProc("SetupCopyOEMInfW")
	procUpdateDriverForPlugAndPlayDevicesW = modnewdev.NewProc("UpdateDriverForPlugAndPlayDevicesW")

	// setupapi — device-interface enumeration (open by DeviceInterfaceGUID)
	procSetupDiGetClassDevsW             = modsetupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces      = modsetupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetailW = modsetupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList     = modsetupapi.NewProc("SetupDiDestroyDeviceInfoList")

	// winusb — the transport (control/bulk transfers)
	modwinusb                        = windows.NewLazySystemDLL("winusb.dll")
	procWinUsbInitialize             = modwinusb.NewProc("WinUsb_Initialize")
	procWinUsbFree                   = modwinusb.NewProc("WinUsb_Free")
	procWinUsbReadPipe               = modwinusb.NewProc("WinUsb_ReadPipe")
	procWinUsbWritePipe              = modwinusb.NewProc("WinUsb_WritePipe")
	procWinUsbSetPipePolicy          = modwinusb.NewProc("WinUsb_SetPipePolicy")
	procWinUsbGetPipePolicy          = modwinusb.NewProc("WinUsb_GetPipePolicy")
	procWinUsbQueryInterfaceSettings = modwinusb.NewProc("WinUsb_QueryInterfaceSettings")
	procWinUsbQueryPipe              = modwinusb.NewProc("WinUsb_QueryPipe")
	procWinUsbGetAssociatedInterface = modwinusb.NewProc("WinUsb_GetAssociatedInterface")
)

// spDeviceInterfaceData is SP_DEVICE_INTERFACE_DATA.
type spDeviceInterfaceData struct {
	cbSize             uint32
	interfaceClassGuid windows.GUID
	flags              uint32
	reserved           uintptr
}

// winusbPipeInfo is WINUSB_PIPE_INFORMATION.
type winusbPipeInfo struct {
	PipeType          uint32 // USBD_PIPE_TYPE
	PipeId            uint8
	_pad              [1]byte
	MaximumPacketSize uint16
	Interval          uint8
	_pad2             [3]byte
}

const (
	// SetupDiGetClassDevs flags
	digcfPresent         = 0x00000002
	digcfDeviceInterface = 0x00000010

	// USBD_PIPE_TYPE
	usbdPipeTypeBulk = 2

	// pipe policy ids (winusbio.h)
	pipePolicyShortPacketTerminate = 0x01
	pipePolicyAutoClearStall       = 0x02
	pipePolicyPipeTransferTimeout  = 0x03
	pipePolicyRawIO                = 0x07
	pipePolicyMaximumTransferSize  = 0x08

	// endpoint direction bit (in the PipeId / bEndpointAddress)
	usbEndpointDirectionMask = 0x80
)

// ---- generic wincrypt blob/struct types (wincrypt.h) ----

// cryptoAPIBlob is CRYPT_INTEGER_BLOB / CRYPT_OBJID_BLOB / CERT_NAME_BLOB /
// CRYPT_HASH_BLOB etc. — all the same shape.
type cryptoAPIBlob struct {
	cbData uint32
	pbData *byte
}

// cryptAlgorithmIdentifier is CRYPT_ALGORITHM_IDENTIFIER.
type cryptAlgorithmIdentifier struct {
	pszObjId   *byte // LPSTR (ANSI OID)
	Parameters cryptoAPIBlob
}

// certExtension is CERT_EXTENSION.
type certExtension struct {
	pszObjId  *byte
	fCritical int32
	// Go inserts 4 bytes padding here to 8-byte-align Value's pointer, matching C.
	Value cryptoAPIBlob
}

// certExtensions is CERT_EXTENSIONS.
type certExtensions struct {
	cExtension  uint32
	rgExtension *certExtension
}

// certEnhKeyUsage is CERT_ENHKEY_USAGE (list of EKU OID strings).
type certEnhKeyUsage struct {
	cUsageIdentifier     uint32
	rgpszUsageIdentifier **byte
}

// cryptKeyProvInfo is CRYPT_KEY_PROV_INFO — links a cert to its key container so
// SignerSignEx can find the private key.
type cryptKeyProvInfo struct {
	pwszContainerName *uint16
	pwszProvName      *uint16
	dwProvType        uint32
	dwFlags           uint32
	cProvParam        uint32
	rgProvParam       uintptr
	dwKeySpec         uint32
}

// ---- SIP (mssip.h) ----

// sipIndirectData is SIP_INDIRECT_DATA.
type sipIndirectData struct {
	Data            cryptAttributeTypeValue
	DigestAlgorithm cryptAlgorithmIdentifier
	Digest          cryptoAPIBlob
}

// cryptAttributeTypeValue is CRYPT_ATTRIBUTE_TYPE_VALUE.
type cryptAttributeTypeValue struct {
	pszObjId *byte
	Value    cryptoAPIBlob
}

// sipSubjectInfo is SIP_SUBJECTINFO (subset; trailing fields we don't set are
// kept for correct size). Passed to the SIP's pfCreate to compute indirect data.
type sipSubjectInfo struct {
	cbSize              uint32
	_pad0               uint32 // align pgSubjectType pointer
	pgSubjectType       *windows.GUID
	hFile               windows.Handle
	pwsFileName         *uint16
	pwsDisplayName      *uint16
	dwReserved1         uint32
	dwIntVersion        uint32
	hProv               uintptr
	DigestAlgorithm     cryptAlgorithmIdentifier
	dwFlags             uint32
	dwEncodingType      uint32
	dwReserved2         uint32
	fdwCAPISettings     uint32
	fdwSecuritySettings uint32
	dwIndex             uint32
	dwUnionChoice       uint32
	pClientData         uintptr
}

// sipDispatchInfo is SIP_DISPATCH_INFO — the SIP function table from CryptSIPLoad.
// Only pfCreate (CryptSIPCreateIndirectData) is used here.
type sipDispatchInfo struct {
	cbSize   uint32
	_pad0    uint32
	hSIP     uintptr
	pfGet    uintptr
	pfPut    uintptr
	pfCreate uintptr // CryptSIPCreateIndirectData
	pfVerify uintptr
	pfRemove uintptr
}

// ---- SIGNER_* (mssign32; not in any header — defined per MS Learn) ----

type signerFileInfo struct {
	cbSize       uint32
	_pad0        uint32
	pwszFileName *uint16
	hFile        windows.Handle
}

type signerSubjectInfo struct {
	cbSize          uint32
	_pad0           uint32
	pdwIndex        *uint32
	dwSubjectChoice uint32
	_pad1           uint32
	pSignerFileInfo *signerFileInfo // union { SIGNER_FILE_INFO*; SIGNER_BLOB_INFO* }
}

type signerCertStoreInfo struct {
	cbSize       uint32
	_pad0        uint32
	pSigningCert *windows.CertContext
	dwCertPolicy uint32
	_pad1        uint32
	hCertStore   windows.Handle
}

type signerCert struct {
	cbSize         uint32
	dwCertChoice   uint32
	pCertStoreInfo *signerCertStoreInfo // union member for SIGNER_CERT_STORE
	hwnd           uintptr
}

type signerSignatureInfo struct {
	cbSize            uint32
	algidHash         uint32 // ALG_ID
	dwAttrChoice      uint32
	_pad0             uint32
	pAttrAuthcode     uintptr // union; unused (SIGNER_NO_ATTR)
	psAuthenticated   uintptr
	psUnauthenticated uintptr
}

// ---- constants ----

const (
	// encoding
	pkcs7ASNEncoding = 0x00010000
	x509ASNEncoding  = 0x00000001

	// provider (advapi32 CryptAcquireContext)
	provRSAAES         = 24 // PROV_RSA_AES
	cryptNewKeyset     = 0x00000008
	cryptMachineKeyset = 0x00000020
	atSignature        = 2 // AT_SIGNATURE

	// CertStrToNameW
	certX500NameStr = 3

	// cert context property ids
	certKeyProvInfoPropID  = 2
	certFriendlyNamePropID = 11

	// CertCreateSelfSignCertificate flags
	certCreateSelfSignNoKeyInfo = 2 // not used (we attach key prov info)

	// cert store add disposition
	certStoreAddReplaceExisting = 3

	// signer
	signerCertStore       = 2 // dwCertChoice = SIGNER_CERT_STORE
	signerSubjectFile     = 1 // dwSubjectChoice = SIGNER_SUBJECT_FILE
	signerNoAttr          = 0 // dwAttrChoice = SIGNER_NO_ATTR
	signerCertPolicyChain = 2

	// ALG_ID for SHA-256 (CALG_SHA_256)
	calgSHA256 = 0x0000800c

	// catalog open flags
	cryptcatOpenCreateNew = 0x00000001
	cryptcatOpenAlways    = 0x00000002
	cryptcatVersion1      = 0x100

	// catalog attribute flags (CRYPTCAT_ATTR_*)
	cryptcatAttrAuthenticated = 0x10000000
	cryptcatAttrNameASCII     = 0x00000001
	cryptcatAttrDataASCII     = 0x00010000

	// BCRYPT SHA-256 algorithm id (for CryptCATAdminAcquireContext2 hash policy)
	bcryptSHA256Algorithm = "SHA256"

	// well-known OIDs
	oidRSASHA256RSA     = "1.2.840.113549.1.1.11"  // szOID_RSA_SHA256RSA
	oidSHA256           = "2.16.840.1.101.3.4.2.1" // szOID_NIST_sha256
	oidSHA1             = "1.3.14.3.2.26"          // szOID_OIWSEC_sha1
	oidCodeSigningEKU   = "1.3.6.1.5.5.7.3.3"      // szOID_PKIX_KP_CODE_SIGNING
	oidEnhancedKeyUsage = "2.5.29.37"              // szOID_ENHANCED_KEY_USAGE

	// CryptEncodeObject struct type for an EKU list (X509_ENHANCED_KEY_USAGE).
	x509EnhancedKeyUsage = 36

	// setupapi SetupCopyOEMInf
	spcopyStyleQuiet = 0x0000 // SP_COPY_NEWER not needed; 0 = default copy

	// UpdateDriverForPlugAndPlayDevices flags
	installFlagForce = 0x00000001 // INSTALLFLAG_FORCE
)

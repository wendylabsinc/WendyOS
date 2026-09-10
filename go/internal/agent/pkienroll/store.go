package pkienroll

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// Directory and file names of the pki-core identity, kept deliberately
// separate from the Certificate Authority Service (CAS) triple in the parent
// directory. The CAS files are /etc/wendy-agent/{device-key.pem,device.pem,
// ca.pem} and are read by the cloud dialers, the agent's inbound mTLS server
// and the mesh; nothing in this package touches them. The names repeat inside
// the subdirectory rather than being prefixed, so a reader who knows the CAS
// layout can read this one without a second convention to learn.
const (
	// StoreSubdir is appended to the agent's config path.
	StoreSubdir = "pki"

	keyFileName   = "device-key.pem"
	leafFileName  = "device.pem"
	chainFileName = "chain.pem"

	// metaFileName records the tenant and frontend the identity was enrolled
	// at. It exists because renewal has to go back to the same tenant, and
	// after a restart there is no staged credential file left to read it from:
	// the file is deleted once redeemed, exactly as enrollment.json is.
	metaFileName = "identity.json"

	// keyMode is 0600 and not 0400. The CAS path writes 0400 on first generate
	// and 0600 through WritePEMFiles, so the mode there tells you which code
	// path wrote last; one mode here means a renewal never has to widen it.
	keyMode  = 0o600
	certMode = 0o644
	dirMode  = 0o700
)

// Metadata is what renewal needs and the certificate does not carry in a form
// worth re-deriving: which tenant issued it and which frontend to go back to.
type Metadata struct {
	TenantUUID  string `json:"tenantUUID"`
	CSREndpoint string `json:"csrEndpoint"`
	// DeviceName is the SPIFFE name the issued SAN carried. Stored for logging
	// and for operator commands that report what this device's identity is;
	// renewal reads the name off the presented certificate, not from here.
	DeviceName string `json:"deviceName"`
}

// Material is the stored pki-core identity: a leaf, the intermediates below it,
// and the private key. It mirrors the shape ProvisioningCerts returns for the
// CAS triple so the dialer can treat the two interchangeably.
type Material struct {
	LeafPEM  string
	ChainPEM string
	// KeyData is a copy the caller owns and should zero after use.
	KeyData []byte
}

// Store holds the pki-core PEM triple under <configPath>/pki.
type Store struct {
	dir string
}

// NewStore returns the store rooted at <configPath>/pki.
func NewStore(configPath string) *Store {
	return &Store{dir: filepath.Join(configPath, StoreSubdir)}
}

// Dir is the directory the triple lives in.
func (s *Store) Dir() string { return s.dir }

// KeyPath, LeafPath and ChainPath are exposed because operator-facing messages
// and logs name the files, and a hand-built path elsewhere would drift.
func (s *Store) KeyPath() string   { return filepath.Join(s.dir, keyFileName) }
func (s *Store) LeafPath() string  { return filepath.Join(s.dir, leafFileName) }
func (s *Store) ChainPath() string { return filepath.Join(s.dir, chainFileName) }
func (s *Store) MetaPath() string  { return filepath.Join(s.dir, metaFileName) }

// SaveMetadata records the tenant and frontend this identity belongs to.
func (s *Store) SaveMetadata(m Metadata) error {
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return fmt.Errorf("creating pki identity directory: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding pki identity metadata: %w", err)
	}
	if err := atomicfile.Write(s.MetaPath(), append(data, '\n'), certMode); err != nil {
		return fmt.Errorf("writing pki identity metadata: %w", err)
	}
	return nil
}

// LoadMetadata reads it back. A missing file yields the zero Metadata and no
// error: a device that was never enrolled has none, and the renewer treats an
// empty tenant as "nothing to renew".
func (s *Store) LoadMetadata() (Metadata, error) {
	data, err := os.ReadFile(s.MetaPath())
	if os.IsNotExist(err) {
		return Metadata{}, nil
	}
	if err != nil {
		return Metadata{}, fmt.Errorf("reading pki identity metadata: %w", err)
	}
	var m Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return Metadata{}, fmt.Errorf("decoding pki identity metadata: %w", err)
	}
	return m, nil
}

// LoadOrGenerateKey returns the device's pki-core private key, generating and
// persisting an ECDSA P-256 key on first call and reusing it afterwards.
//
// The key is generated ON THE DEVICE and never leaves it: pki-core does not
// implement server-side key generation, and enrolling from a laptop would put
// the device's key on the laptop. It is a SEPARATE key from the CAS
// device-key.pem — sharing one key across the two identities would make them
// one identity wearing two certificates, so a compromise of either would be a
// compromise of both.
//
// The returned slice is the caller's to zero.
func (s *Store) LoadOrGenerateKey() ([]byte, error) {
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return nil, fmt.Errorf("creating pki identity directory: %w", err)
	}
	if data, err := os.ReadFile(s.KeyPath()); err == nil && len(data) > 0 {
		return data, nil
	}
	keyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("generating pki identity key: %w", err)
	}
	if err := atomicfile.Write(s.KeyPath(), []byte(keyPEM), keyMode); err != nil {
		return nil, fmt.Errorf("writing pki identity key: %w", err)
	}
	return []byte(keyPEM), nil
}

// ErrNoIdentity says the store holds no usable identity. It is an ordinary
// state, not a failure: a device that has never been enrolled against pki-core
// reports it, and every caller answers by falling back to the CAS identity.
var ErrNoIdentity = errors.New("no pki-core device identity stored")

// Load reads the stored triple. It returns ErrNoIdentity when the leaf or the
// key is missing or empty, so a half-written store (a key generated but never
// enrolled) reads as absent rather than as broken.
func (s *Store) Load() (Material, error) {
	leaf, err := os.ReadFile(s.LeafPath())
	if err != nil || len(leaf) == 0 {
		return Material{}, ErrNoIdentity
	}
	key, err := os.ReadFile(s.KeyPath())
	if err != nil || len(key) == 0 {
		return Material{}, ErrNoIdentity
	}
	// An absent chain file is legitimate: pki-core returns a leaf alone when
	// there are no intermediates below the issuing certificate authority.
	chain, err := os.ReadFile(s.ChainPath())
	if err != nil && !os.IsNotExist(err) {
		return Material{}, fmt.Errorf("reading pki identity chain: %w", err)
	}
	return Material{LeafPEM: string(leaf), ChainPEM: string(chain), KeyData: key}, nil
}

// Has reports whether a usable identity is stored.
func (s *Store) Has() bool {
	_, err := s.Load()
	return err == nil
}

// Save writes the issued leaf and chain, atomically and leaf last.
//
// Order matters: Load treats a missing leaf as "no identity", so writing the
// chain first means a crash between the two writes leaves the store readable as
// absent rather than as an identity whose chain does not match its leaf. The
// key is written by LoadOrGenerateKey and is never rewritten here, because the
// key that signed the CSR is the key the issued leaf attests.
func (s *Store) Save(result Result) error {
	if result.LeafPEM == "" {
		return errors.New("refusing to store an empty pki identity leaf")
	}
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return fmt.Errorf("creating pki identity directory: %w", err)
	}
	if result.ChainPEM != "" {
		if err := atomicfile.Write(s.ChainPath(), []byte(result.ChainPEM), certMode); err != nil {
			return fmt.Errorf("writing pki identity chain: %w", err)
		}
	} else if err := os.Remove(s.ChainPath()); err != nil && !os.IsNotExist(err) {
		// A leaf-only issuance must not inherit the previous chain: an
		// intermediate that does not sign the new leaf fails the handshake in
		// a way that looks like a server-side refusal.
		return fmt.Errorf("removing stale pki identity chain: %w", err)
	}
	if err := atomicfile.Write(s.LeafPath(), []byte(result.LeafPEM), certMode); err != nil {
		return fmt.Errorf("writing pki identity leaf: %w", err)
	}
	return nil
}

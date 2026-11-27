// Package agentvmfixture is a disposable, untrusted test issuer for an isolated
// private-agent VM. It must never be imported by the agent or used for customers.
package agentvmfixture

import (
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

const Kind = "UNTRUSTED-PRIVATE-AGENT-VM-FIXTURE-v1"
const App = "com.wendy.world.vmfixture"
const ClockDomain = "simulation:agent-vm-fixture"

var SampleTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type Public struct {
	PairID              string    `json:"pair_id,omitempty"`
	LeafSerial          uint64    `json:"leaf_serial,omitempty"`
	Kind                string    `json:"kind"`
	Tenant              string    `json:"tenant"`
	DeviceID            string    `json:"device_id"`
	Principal           string    `json:"principal"`
	RootDER             []byte    `json:"root_der"`
	IssuerDER           []byte    `json:"issuer_der"`
	RootSHA256          string    `json:"root_sha256"`
	IssuerSHA256        string    `json:"issuer_sha256"`
	ConfigPath          string    `json:"config_path"`
	SimulationClockPath string    `json:"simulation_clock_path"`
	NotBefore           time.Time `json:"not_before"`
	NotAfter            time.Time `json:"not_after"`
}

// secret never contains a device or operator key. The actual agent generates
// the device key during normal ACME enrollment; the issuer sees its CSR only.
type secret struct {
	PairID    string `json:"pair_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	Kind      string `json:"kind"`
	IssuerKey []byte `json:"issuer_key_pkcs8"`
	EABKeyID  string `json:"eab_key_id"`
	EABKey    []byte `json:"eab_hmac_key"`
}

type authority struct {
	public    Public
	secret    secret
	issuer    *x509.Certificate
	key       *mldsa.PrivateKey
	directory string
}

func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeNew(path, append(b, '\n'))
}

// Initialize requires a fresh empty directory. It creates only this fixture's
// material and commissioned configuration, never device provisioning state.
func Initialize(directory, stateDirectory string) (Public, error) {
	return initialize(directory, stateDirectory, true)
}

// InitializeEnrollment creates the disposable issuer and public identity before
// a scene exists. It reserves the usual configuration paths but writes neither
// World policy nor a source clock. A later explicit commissioning step activates
// World against the observed live scene; ordinary agent enrollment can run first.
func InitializeEnrollment(directory, stateDirectory string) (Public, error) {
	return initialize(directory, stateDirectory, false)
}

func initialize(directory, stateDirectory string, scalarWorld bool) (Public, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || !filepath.IsAbs(stateDirectory) || filepath.Clean(stateDirectory) != stateDirectory || directory == stateDirectory {
		return Public{}, errors.New("distinct clean absolute fixture and state directories required")
	}
	if err := hostpath.EnsureDirectory(directory, 0700, true); err != nil {
		return Public{}, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		return Public{}, errors.New("fixture directory must be empty; never reuse real agent state")
	}
	if err = hostpath.EnsureDirectory(stateDirectory, 0700, true); err != nil {
		return Public{}, err
	}
	stateEntries, err := os.ReadDir(stateDirectory)
	if err != nil || len(stateEntries) != 0 {
		return Public{}, errors.New("fixture state directory must be empty")
	}
	private := filepath.Join(directory, ".vm-fixture")
	if err = os.Mkdir(private, 0700); err != nil {
		return Public{}, err
	}
	now := time.Now().UTC()
	pki, err := newFixturePKI(now)
	if err != nil {
		return Public{}, err
	}
	return writeNode(directory, stateDirectory, scalarWorld, pki, uuid.NewString(), uuid.NewString(), "", 0, writeJSON)
}

type fixturePKI struct {
	rootDER, issuerDER, issuerPKCS8 []byte
	now                             time.Time
}

func newFixturePKI(now time.Time) (fixturePKI, error) {
	rootKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		return fixturePKI{}, err
	}
	issuerKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		return fixturePKI{}, err
	}
	ca := func(serial int64, name string) *x509.Certificate {
		return &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(2 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	}
	root := ca(1, "UNTRUSTED DISPOSABLE VM ROOT")
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootKey.Public(), rootKey)
	if err != nil {
		return fixturePKI{}, err
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		return fixturePKI{}, err
	}
	issuerDER, err := x509.CreateCertificate(rand.Reader, ca(2, "UNTRUSTED DISPOSABLE VM ISSUER"), root, issuerKey.Public(), rootKey)
	if err != nil {
		return fixturePKI{}, err
	}
	// Deliberately do not persist the root private key.
	issuerPKCS8, err := x509.MarshalPKCS8PrivateKey(issuerKey)
	if err != nil {
		return fixturePKI{}, err
	}
	return fixturePKI{rootDER, issuerDER, issuerPKCS8, now}, nil
}

func writeNode(directory, stateDirectory string, scalarWorld bool, material fixturePKI, tenant, device, pairID string, serial uint64, write func(string, any) error) (Public, error) {
	private := filepath.Join(directory, ".vm-fixture")
	now, rootDER, issuerDER, issuerPKCS8 := material.now, material.rootDER, material.issuerDER, material.issuerPKCS8
	mac := make([]byte, 32)
	if _, err := rand.Read(mac); err != nil {
		return Public{}, err
	}
	p := Public{PairID: pairID, LeafSerial: serial, Kind: Kind, Tenant: tenant, DeviceID: device, Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device, RootDER: rootDER, IssuerDER: issuerDER, RootSHA256: digest(rootDER), IssuerSHA256: digest(issuerDER), ConfigPath: filepath.Join(directory, "world.json"), SimulationClockPath: filepath.Join(directory, "simclock.json"), NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(time.Hour)}
	var grants []agentmodule.Grant
	for _, name := range []string{"gas", "temperature"} {
		grants = append(grants, agentmodule.Grant{App: App, Service: name, Publish: []serviceorigin.PublishScope{{ComponentType: "wendy.sensor.scalar.v1", Stream: name}}})
	}
	cfg := agentmodule.FileConfig{Policy: agentmodule.Policy{Version: 1, Tenant: tenant, Site: "office", Device: p.Principal, ExecutionDomain: "simulation", SampleClockDomain: ClockDomain, Frames: []world.FrameRef{{ID: "office-floor", Revision: "vm-fixture-1"}}, Grants: grants, NotBefore: p.NotBefore, NotAfter: p.NotAfter}, RootsDER: [][]byte{rootDER}, SocketDirectory: filepath.Join(stateDirectory, "sockets"), JournalPath: filepath.Join(stateDirectory, "journal", "journal.db"), SimulationClockFile: p.SimulationClockPath}
	files := []struct {
		path  string
		value any
	}{
		{filepath.Join(private, "authority.json"), secret{PairID: pairID, DeviceID: pairedDeviceID(pairID, device), Kind: Kind, IssuerKey: issuerPKCS8, EABKeyID: uuid.NewString(), EABKey: mac}},
		{filepath.Join(directory, "fixture-public.json"), p},
	}
	if scalarWorld {
		files = append(files, struct {
			path  string
			value any
		}{p.ConfigPath, cfg}, struct {
			path  string
			value any
		}{p.SimulationClockPath, struct {
			Domain string    `json:"domain"`
			Now    time.Time `json:"now"`
		}{ClockDomain, SampleTime}})
	}
	for _, file := range files {
		if err := write(file.path, file.value); err != nil {
			return Public{}, err
		}
	}
	return p, nil
}

func readPublic(directory string) (Public, error) {
	var p Public
	b, err := agentmodule.ReadCommissionedFile(filepath.Join(directory, "fixture-public.json"), 64<<10)
	if err != nil || jws.DecodeStrict(b, &p) != nil || p.Kind != Kind || p.RootSHA256 != digest(p.RootDER) || p.IssuerSHA256 != digest(p.IssuerDER) || p.Principal != "spiffe://wendy.sh/tenant/"+p.Tenant+"/device/"+p.DeviceID || p.ConfigPath != filepath.Join(directory, "world.json") || p.SimulationClockPath != filepath.Join(directory, "simclock.json") {
		return Public{}, errors.New("valid generated fixture public manifest required")
	}
	for _, s := range []string{p.Tenant, p.DeviceID} {
		u, e := uuid.Parse(s)
		if e != nil || u.String() != s {
			return Public{}, errors.New("canonical fixture UUID required")
		}
	}
	if p.PairID != "" {
		if err := validatePairMember(directory, p, b); err != nil {
			return Public{}, err
		}
	} else if p.LeafSerial != 0 {
		return Public{}, errors.New("unpaired fixture serial refused")
	}
	return p, nil
}

func load(directory string) (*authority, error) {
	p, err := readPublic(directory)
	if err != nil {
		return nil, err
	}
	b, err := agentmodule.ReadCommissionedFile(filepath.Join(directory, ".vm-fixture", "authority.json"), 64<<10)
	var s secret
	if err != nil || jws.DecodeStrict(b, &s) != nil || s.Kind != Kind || len(s.EABKey) != 32 || s.EABKeyID == "" {
		return nil, errors.New("generated fixture issuer material unavailable")
	}
	if s.PairID != p.PairID || s.DeviceID != pairedDeviceID(p.PairID, p.DeviceID) {
		return nil, errors.New("fixture issuer target mismatch")
	}
	key, err := x509.ParsePKCS8PrivateKey(s.IssuerKey)
	if err != nil {
		return nil, errors.New("invalid fixture issuer key")
	}
	k, ok := key.(*mldsa.PrivateKey)
	if !ok || k.PublicKey().Parameters() != mldsa.MLDSA65() {
		return nil, errors.New("fixture ML-DSA-65 issuer required")
	}
	issuer, err := x509.ParseCertificate(p.IssuerDER)
	if err != nil || !k.PublicKey().Equal(issuer.PublicKey) {
		return nil, errors.New("fixture issuer mismatch")
	}
	if !time.Now().Before(p.NotAfter) {
		return nil, errors.New("fixture expired; create a new isolated fixture")
	}
	return &authority{public: p, secret: s, issuer: issuer, key: k, directory: directory}, nil
}

func receiptPath(directory, name string) string { return filepath.Join(directory, ".vm-fixture", name) }
func safeError(stage string) error              { return fmt.Errorf("test ACME %s refused", stage) }

func pairedDeviceID(pairID, device string) string {
	if pairID == "" {
		return ""
	}
	return device
}

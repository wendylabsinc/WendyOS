package agentvmfixture

import (
	"bytes"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

const PairKind = "UNTRUSTED-PRIVATE-AGENT-PAIR-FIXTURE-v1"

type PairNode struct {
	Directory      string `json:"directory"`
	StateDirectory string `json:"state_directory"`
	Public         Public `json:"public"`
	PublicSHA256   string `json:"public_sha256"`
}

// PairPublic is written only after both node issuers exist. A staged node needs
// this public parent manifest and its own directory, never its peer's secrets.
type PairPublic struct {
	Kind         string      `json:"kind"`
	PairID       string      `json:"pair_id"`
	Tenant       string      `json:"tenant"`
	RootSHA256   string      `json:"root_sha256"`
	IssuerSHA256 string      `json:"issuer_sha256"`
	Nodes        [2]PairNode `json:"nodes"`
}

// InitializePairEnrollment creates test issuers under directory/a and /b.
// Neither World policy nor device keys are created. Failure may leave partial
// material; its missing completion marker blocks loading. Use fresh paths after
// failure, rather than repairing or reusing that material.
func InitializePairEnrollment(directory string, states [2]string) (PairPublic, error) {
	return initializePairEnrollment(directory, states, writeJSON)
}

func pairPaths(directory string, states [2]string) bool {
	paths := []string{directory, states[0], states[1]}
	for i, p := range paths {
		if !cleanAbsolute(p) {
			return false
		}
		for j := 0; j < i; j++ {
			q := paths[j]
			if p == q || strings.HasPrefix(p, q+string(filepath.Separator)) || strings.HasPrefix(q, p+string(filepath.Separator)) {
				return false
			}
		}
	}
	return true
}

func initializePairEnrollment(directory string, states [2]string, write func(string, any) error) (PairPublic, error) {
	if !pairPaths(directory, states) || write == nil {
		return PairPublic{}, errors.New("distinct nonoverlapping clean absolute pair and state directories required")
	}
	for _, p := range []string{directory, states[0], states[1]} {
		if err := hostpath.EnsureDirectory(p, 0700, true); err != nil {
			return PairPublic{}, err
		}
		entries, err := os.ReadDir(p)
		if err != nil || len(entries) != 0 {
			return PairPublic{}, errors.New("pair and state directories must be empty; never reuse prior material")
		}
	}
	material, err := newFixturePKI(time.Now().UTC())
	if err != nil {
		return PairPublic{}, err
	}
	pair := PairPublic{Kind: PairKind, PairID: uuid.NewString(), Tenant: uuid.NewString(), RootSHA256: digest(material.rootDER), IssuerSHA256: digest(material.issuerDER)}
	ids := [2]string{uuid.NewString(), uuid.NewString()}
	if ids[0] == ids[1] {
		return PairPublic{}, errors.New("device identifier collision")
	}
	for i, name := range []string{"a", "b"} {
		dir := filepath.Join(directory, name)
		if err = os.Mkdir(dir, 0700); err != nil {
			return PairPublic{}, err
		}
		if err = os.Mkdir(filepath.Join(dir, ".vm-fixture"), 0700); err != nil {
			return PairPublic{}, err
		}
		pub, err := writeNode(dir, states[i], false, material, pair.Tenant, ids[i], pair.PairID, uint64(i+3), write)
		if err != nil {
			return PairPublic{}, err
		}
		raw, err := json.MarshalIndent(pub, "", "  ")
		if err != nil {
			return PairPublic{}, err
		}
		pair.Nodes[i] = PairNode{dir, states[i], pub, digest(append(raw, '\n'))}
	}
	if err = write(filepath.Join(directory, "pair-public.json"), pair); err != nil {
		return PairPublic{}, err
	}
	return pair, nil
}

func canonicalUUID(s string) bool { u, e := uuid.Parse(s); return e == nil && u.String() == s }
func exactDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

func readPair(directory string) (PairPublic, error) {
	var pair PairPublic
	if !cleanAbsolute(directory) {
		return pair, errors.New("clean absolute pair directory required")
	}
	raw, err := agentmodule.ReadCommissionedFile(filepath.Join(directory, "pair-public.json"), 128<<10)
	if err != nil || jws.DecodeStrict(raw, &pair) != nil || pair.Kind != PairKind || !canonicalUUID(pair.PairID) || !canonicalUUID(pair.Tenant) || !exactDigest(pair.RootSHA256) || !exactDigest(pair.IssuerSHA256) {
		return PairPublic{}, errors.New("complete protected pair manifest required")
	}
	// encoding/json otherwise discards excess elements of a fixed Go array.
	var shape struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if json.Unmarshal(raw, &shape) != nil || len(shape.Nodes) != 2 {
		return PairPublic{}, errors.New("exactly two completed pair nodes required")
	}
	if !pairPaths(directory, [2]string{pair.Nodes[0].StateDirectory, pair.Nodes[1].StateDirectory}) {
		return PairPublic{}, errors.New("invalid pair paths")
	}
	for i, name := range []string{"a", "b"} {
		node := pair.Nodes[i]
		p := node.Public
		if node.Directory != filepath.Join(directory, name) || p.Kind != Kind || p.PairID != pair.PairID || p.LeafSerial != uint64(i+3) || p.Tenant != pair.Tenant || !canonicalUUID(p.DeviceID) || p.Principal != "spiffe://wendy.sh/tenant/"+p.Tenant+"/device/"+p.DeviceID || p.RootSHA256 != pair.RootSHA256 || p.IssuerSHA256 != pair.IssuerSHA256 || digest(p.RootDER) != pair.RootSHA256 || digest(p.IssuerDER) != pair.IssuerSHA256 || p.ConfigPath != filepath.Join(node.Directory, "world.json") || p.SimulationClockPath != filepath.Join(node.Directory, "simclock.json") || !p.NotAfter.After(p.NotBefore) {
			return PairPublic{}, errors.New("pair node metadata mismatch")
		}
		canonical, err := json.MarshalIndent(p, "", "  ")
		if err != nil || node.PublicSHA256 != digest(append(canonical, '\n')) {
			return PairPublic{}, errors.New("pair node digest mismatch")
		}
	}
	a, b := pair.Nodes[0].Public, pair.Nodes[1].Public
	if a.DeviceID == b.DeviceID || !bytes.Equal(a.RootDER, b.RootDER) || !bytes.Equal(a.IssuerDER, b.IssuerDER) || !a.NotBefore.Equal(b.NotBefore) || !a.NotAfter.Equal(b.NotAfter) {
		return PairPublic{}, errors.New("pair identities or trust mismatch")
	}
	return pair, nil
}

func validatePairMember(directory string, p Public, raw []byte) error {
	pair, err := readPair(filepath.Dir(directory))
	if err != nil {
		return err
	}
	for _, node := range pair.Nodes {
		if node.Directory == directory && reflect.DeepEqual(node.Public, p) && node.PublicSHA256 == digest(raw) {
			return nil
		}
	}
	return errors.New("node is not an exact completed pair member")
}

type PeerCertificate struct {
	DeviceID            string `json:"device_id"`
	Principal           string `json:"principal"`
	LeafSHA256          string `json:"leaf_sha256"`
	DeviceSPKISHA256    string `json:"device_spki_sha256"`
	LeafSerial          uint64 `json:"leaf_serial"`
	IssuedReceiptSHA256 string `json:"issued_receipt_sha256"`
}
type PairPeers struct {
	Kind          string             `json:"kind"`
	PairID        string             `json:"pair_id"`
	Tenant        string             `json:"tenant"`
	RootSHA256    string             `json:"root_sha256"`
	IssuerSHA256  string             `json:"issuer_sha256"`
	Nodes         [2]PeerCertificate `json:"nodes"`
	AuthorityTime string             `json:"authority_time"`
}

// ReadPairPeers reads public evidence only after both normal enrollments.
// This host-wall-time fixture check is not World authenticated time or current
// peer authority. It neither issues keys nor installs policy.
func ReadPairPeers(directory string) (PairPeers, error) {
	pair, err := readPair(directory)
	if err != nil {
		return PairPeers{}, err
	}
	out := PairPeers{Kind: PairKind, PairID: pair.PairID, Tenant: pair.Tenant, RootSHA256: pair.RootSHA256, IssuerSHA256: pair.IssuerSHA256, AuthorityTime: "fixture-host-wall-time-only"}
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: pair.Tenant, Roots: [][]byte{pair.Nodes[0].Public.RootDER}})
	if err != nil {
		return PairPeers{}, err
	}
	now := time.Now().UTC()
	for i, node := range pair.Nodes {
		p, err := readPublic(node.Directory)
		if err != nil {
			return PairPeers{}, err
		}
		raw, err := agentmodule.ReadCommissionedFile(receiptPath(node.Directory, "issued.json"), 64<<10)
		var r IssuedReceipt
		if err != nil || jws.DecodeStrict(raw, &r) != nil || r.Kind != Kind || r.Principal != p.Principal || r.LeafSHA256 != digest(r.LeafDER) || !exactDigest(r.DeviceSPKISHA256) || !exactDigest(r.CSRSHA256) || !r.CSRVerified || !r.EABVerified || !r.AccountJWSVerified {
			return PairPeers{}, errors.New("both complete public enrollment receipts required")
		}
		id, err := certs.Verify([][]byte{r.LeafDER, p.IssuerDER, p.RootDER}, now, now)
		if err != nil || id.Principal != p.Principal || id.CertificateSHA256 != r.LeafSHA256 || id.SigningKeySHA256 != r.DeviceSPKISHA256 {
			return PairPeers{}, errors.New("paired enrollment certificate mismatch")
		}
		serial, err := certificateSerial(r.LeafDER)
		if err != nil || serial != p.LeafSerial {
			return PairPeers{}, errors.New("paired leaf serial mismatch")
		}
		out.Nodes[i] = PeerCertificate{p.DeviceID, p.Principal, r.LeafSHA256, r.DeviceSPKISHA256, serial, digest(raw)}
	}
	if out.Nodes[0].LeafSHA256 == out.Nodes[1].LeafSHA256 || out.Nodes[0].DeviceSPKISHA256 == out.Nodes[1].DeviceSPKISHA256 {
		return PairPeers{}, errors.New("independent agent keys required")
	}
	return out, nil
}

func certificateSerial(der []byte) (uint64, error) {
	c, e := x509.ParseCertificate(der)
	if e != nil || c.SerialNumber == nil || !c.SerialNumber.IsUint64() || c.SerialNumber.Sign() <= 0 {
		return 0, errors.New("positive fixture certificate serial required")
	}
	return c.SerialNumber.Uint64(), nil
}

package agentvmfixture

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

func pairFixture(t *testing.T) (string, PairPublic, [2]*authority) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "pair")
	pair, err := InitializePairEnrollment(dir, [2]string{filepath.Join(base, "state-a"), filepath.Join(base, "state-b")})
	if err != nil {
		t.Fatal(err)
	}
	var nodes [2]*authority
	for i, n := range pair.Nodes {
		nodes[i], err = load(n.Directory)
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir, pair, nodes
}

func TestPairSharedTrustDistinctAgentEnrollmentAndPublicPeers(t *testing.T) {
	dir, pair, nodes := pairFixture(t)
	a, b := nodes[0], nodes[1]
	if err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		want := os.FileMode(0600)
		if info.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("pair paths are not private regular files/directories")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if a.public.Tenant != b.public.Tenant || !bytes.Equal(a.public.RootDER, b.public.RootDER) || !bytes.Equal(a.public.IssuerDER, b.public.IssuerDER) || !bytes.Equal(a.secret.IssuerKey, b.secret.IssuerKey) {
		t.Fatal("pair did not share only its test trust")
	}
	if a.public.DeviceID == b.public.DeviceID || a.public.Principal == b.public.Principal || a.secret.EABKeyID == b.secret.EABKeyID || bytes.Equal(a.secret.EABKey, b.secret.EABKey) {
		t.Fatal("pair identity or EAB collision")
	}
	if _, err := ReadPairPeers(dir); err == nil {
		t.Fatal("peers existed before enrollment")
	}
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: pair.Tenant, Roots: [][]byte{a.public.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	var keys [2]*mldsa.PrivateKey
	var leaves [2][]byte
	for i, n := range nodes {
		entries, err := os.ReadDir(pair.Nodes[i].StateDirectory)
		if err != nil || len(entries) != 0 {
			t.Fatal("initializer wrote agent state", err)
		}
		for _, p := range []string{n.public.ConfigPath, n.public.SimulationClockPath, filepath.Join(n.directory, "device-key.pem"), receiptPath(n.directory, "issued.json")} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatal("premature policy/key/issuance", err)
			}
		}
		var chain [][]byte
		keys[i], chain = enroll(t, n)
		leaves[i] = chain[0]
		now := time.Now()
		id, err := certs.Verify(chain, now, now)
		if err != nil || id.Principal != n.public.Principal {
			t.Fatal("shared root did not verify exact node", err)
		}
		leaf, err := x509.ParseCertificate(chain[0])
		if err != nil || leaf.SerialNumber.Uint64() != uint64(i+3) || !keys[i].PublicKey().Equal(leaf.PublicKey) {
			t.Fatal("distinct serial or CSR key missing", err)
		}
		if i == 0 {
			if _, err = ReadPairPeers(dir); err == nil {
				t.Fatal("partial enrollment accepted as pair")
			}
		}
	}
	if keys[0].PublicKey().Equal(keys[1].PublicKey()) || bytes.Equal(leaves[0], leaves[1]) {
		t.Fatal("agent keys were shared")
	}
	peers, err := ReadPairPeers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if peers.PairID != pair.PairID || peers.AuthorityTime != "fixture-host-wall-time-only" {
		t.Fatal("incorrect qualification")
	}
	for i, p := range peers.Nodes {
		if p.LeafSHA256 != digest(leaves[i]) || p.Principal != nodes[i].public.Principal || p.LeafSerial != uint64(i+3) {
			t.Fatal("peer fingerprint changed")
		}
	}
	public, err := JSONReceipt(pair)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if bytes.Contains(public, []byte(n.secret.EABKeyID)) || bytes.Contains(public, []byte(base64.StdEncoding.EncodeToString(n.secret.EABKey))) || bytes.Contains(public, []byte(base64.StdEncoding.EncodeToString(n.secret.IssuerKey))) {
			t.Fatal("secret exported in pair metadata")
		}
	}
	for _, n := range nodes {
		if err := Serve(context.Background(), n.directory, "0.0.0.0:0", filepath.Join(n.directory, "acme-enrollment.json"), time.Second, nil); err == nil {
			t.Fatal("external issuer listener accepted")
		}
		if err := Serve(context.Background(), n.directory, "127.0.0.1:0", filepath.Join(n.directory, "acme-enrollment.json"), time.Second, nil); err == nil {
			t.Fatal("node reissued after successful enrollment")
		}
	}
}

func TestPairNodeLoadsWithoutPeerPrivateMaterial(t *testing.T) {
	dir, pair, _ := pairFixture(t)
	other := pair.Nodes[1].Directory
	if err := os.Rename(other, other+"-not-staged"); err != nil {
		t.Fatal(err)
	}
	if _, err := load(pair.Nodes[0].Directory); err != nil {
		t.Fatal("node required peer directory", err)
	}
	if _, err := ReadPairPeers(dir); err == nil {
		t.Fatal("missing public peer enrollment accepted")
	}
	// An exact completion marker is still mandatory when only this node is staged.
	if err := os.Remove(filepath.Join(dir, "pair-public.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := load(pair.Nodes[0].Directory); err == nil {
		t.Fatal("node loaded without completion")
	}
}

func TestPairRefusesCrossNodeEABOrderAndCSRTarget(t *testing.T) {
	_, _, nodes := pairFixture(t)
	a, b := nodes[0], nodes[1]
	c := client(t, b)
	wrongKey := c.eab()
	mac := hmac.New(sha256.New, a.secret.EABKey)
	_, _ = mac.Write([]byte(wrongKey.Protected + "." + wrongKey.Payload))
	wrongKey.Signature = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if status, _ := c.post("/acme/new-account", map[string]any{"externalAccountBinding": wrongKey}, true); status == 201 {
		t.Fatal("peer EAB key accepted")
	}
	wrongID := c.eab()
	wrongID.Protected = b64(header{Alg: "HS256", URL: c.srv.URL + "/acme/new-account", KID: a.secret.EABKeyID})
	mac = hmac.New(sha256.New, b.secret.EABKey)
	_, _ = mac.Write([]byte(wrongID.Protected + "." + wrongID.Payload))
	wrongID.Signature = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if status, _ := c.post("/acme/new-account", map[string]any{"externalAccountBinding": wrongID}, true); status == 201 {
		t.Fatal("peer EAB identifier accepted")
	}
	c.register()
	if status, _ := c.post("/acme/new-order", map[string]any{"identifiers": []map[string]string{{"type": "permanent-identifier", "value": a.public.DeviceID}}}, false); status == 201 {
		t.Fatal("peer order target accepted")
	}
	c.order()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	peerURI, _ := url.Parse(a.public.Principal)
	for _, template := range []*x509.CertificateRequest{{}, {Subject: pkix.Name{CommonName: a.public.DeviceID}}, {Subject: pkix.Name{CommonName: b.public.DeviceID}, URIs: []*url.URL{peerURI}}, {Subject: pkix.Name{CommonName: b.public.DeviceID}, DNSNames: []string{"foreign.test"}}} {
		csr, err := x509.CreateCertificateRequest(rand.Reader, template, key)
		if err != nil {
			t.Fatal(err)
		}
		if status, _ := c.post("/acme/finalize", map[string]string{"csr": base64.RawURLEncoding.EncodeToString(csr)}, false); status == 200 {
			t.Fatal("wrong or missing paired CSR identity accepted")
		}
	}
	if _, err := os.Stat(receiptPath(b.directory, "issued.json")); !os.IsNotExist(err) {
		t.Fatal("refused request wrote issuance")
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: b.public.DeviceID}}, key)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := c.post("/acme/finalize", map[string]string{"csr": base64.RawURLEncoding.EncodeToString(csr)}, false); status != 200 {
		t.Fatal("exact normal agent CSR refused", status)
	}
	if _, err := os.Stat(receiptPath(a.directory, "issued.json")); !os.IsNotExist(err) {
		t.Fatal("node B wrote node A issuance")
	}
}

func TestPairPartialWritesNeverEnableAnIssuer(t *testing.T) {
	for _, failAt := range []int{1, 2, 3, 4, 5} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(base, "pair")
			states := [2]string{filepath.Join(base, "state-a"), filepath.Join(base, "state-b")}
			writes := 0
			_, err = initializePairEnrollment(dir, states, func(p string, v any) error {
				writes++
				if writes == failAt {
					return errors.New("injected exclusive write failure")
				}
				return writeJSON(p, v)
			})
			if err == nil || writes != failAt {
				t.Fatal("partial create did not fail", err, writes)
			}
			for _, name := range []string{"a", "b"} {
				if _, err := load(filepath.Join(dir, name)); err == nil {
					t.Fatal("partial issuer loaded")
				}
			}
			if _, err := ReadPairPeers(dir); err == nil {
				t.Fatal("partial pair exported")
			}
			if _, err := InitializePairEnrollment(dir, states); err == nil {
				t.Fatal("partial material was silently repaired")
			}
		})
	}
}

func TestPairDirectoryCollisionsAndCompletionTamperingRefuse(t *testing.T) {
	dir, pair, nodes := pairFixture(t)
	before, err := os.ReadFile(filepath.Join(dir, "pair-public.json"))
	if err != nil {
		t.Fatal(err)
	}
	states := [2]string{pair.Nodes[0].StateDirectory, pair.Nodes[1].StateDirectory}
	for _, bad := range [][2]string{{states[0], states[0]}, {dir, states[1]}, {filepath.Join(dir, "nested"), states[1]}, {filepath.Dir(dir), states[1]}} {
		if _, err := InitializePairEnrollment(dir, bad); err == nil {
			t.Fatal("overlapping paths accepted")
		}
	}
	if _, err := InitializePairEnrollment(dir, states); err == nil {
		t.Fatal("pair reinitialized")
	}
	after, err := os.ReadFile(filepath.Join(dir, "pair-public.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("collision overwrote completed pair")
	}
	// Swapping same-issuer secrets must not swap EAB authority between devices.
	secretA, err := os.ReadFile(receiptPath(nodes[0].directory, "authority.json"))
	if err != nil {
		t.Fatal(err)
	}
	secretB, err := os.ReadFile(receiptPath(nodes[1].directory, "authority.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(receiptPath(nodes[1].directory, "authority.json"), secretA, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = load(nodes[1].directory); err == nil {
		t.Fatal("foreign paired issuer credentials accepted")
	}
	if err = os.WriteFile(receiptPath(nodes[1].directory, "authority.json"), secretB, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"pair-id", "digest", "serial", "path", "node-bytes"} {
		t.Run(mode, func(t *testing.T) {
			var changed PairPublic
			if err := json.Unmarshal(before, &changed); err != nil {
				t.Fatal(err)
			}
			pubPath := filepath.Join(nodes[0].directory, "fixture-public.json")
			original, err := os.ReadFile(pubPath)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "pair-id":
				changed.PairID = pair.Tenant
			case "digest":
				changed.Nodes[0].PublicSHA256 = pair.RootSHA256
			case "serial":
				changed.Nodes[0].Public.LeafSerial = 4
			case "path":
				changed.Nodes[0].Directory = nodes[1].directory
			case "node-bytes":
				if err = os.WriteFile(pubPath, append(original, ' '), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "pair-public.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = load(nodes[0].directory); err == nil {
				t.Fatal("changed exact pair metadata accepted")
			}
			if err = os.WriteFile(filepath.Join(dir, "pair-public.json"), before, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(pubPath, original, 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
	if got, err := readPair(dir); err != nil || !reflect.DeepEqual(got, pair) {
		t.Fatal("restored exact public manifest refused", err)
	}
}

func TestPairPeerExchangeRefusesChangedPublicIssuance(t *testing.T) {
	dir, pair, nodes := pairFixture(t)
	for _, n := range nodes {
		enroll(t, n)
	}
	target := receiptPath(nodes[1].directory, "issued.json")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"foreign-node", "spki", "flag", "digest"} {
		t.Run(mode, func(t *testing.T) {
			var r IssuedReceipt
			if err := json.Unmarshal(original, &r); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "foreign-node":
				r.Principal = pair.Nodes[0].Public.Principal
			case "spki":
				r.DeviceSPKISHA256 = pair.RootSHA256
			case "flag":
				r.CSRVerified = false
			case "digest":
				r.LeafSHA256 = pair.RootSHA256
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(target, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = ReadPairPeers(dir); err == nil {
				t.Fatal("invalid enrollment receipt accepted")
			}
		})
	}
	if err = os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadPairPeers(dir); err != nil {
		t.Fatal(err)
	}
	// Public fingerprint exchange must not open either private issuer/EAB file.
	for _, n := range nodes {
		if err := os.Remove(receiptPath(n.directory, "authority.json")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = ReadPairPeers(dir); err != nil {
		t.Fatal("public exchange required issuer secrets", err)
	}
}

func TestPairRefusesOccupiedOrUnsafeStateBeforeIssuerWrite(t *testing.T) {
	for _, mode := range []string{"occupied", "symlink", "writable"} {
		t.Run(mode, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(base, "pair")
			states := [2]string{filepath.Join(base, "state-a"), filepath.Join(base, "state-b")}
			switch mode {
			case "occupied":
				if err = os.Mkdir(states[1], 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(states[1], "existing-state"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(base, "target")
				if err = os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(target, states[1]); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err = os.Mkdir(states[1], 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.Chmod(states[1], 0777); err != nil {
					t.Fatal(err)
				}
			}
			writes := 0
			_, err = initializePairEnrollment(dir, states, func(p string, v any) error { writes++; return writeJSON(p, v) })
			if err == nil || writes != 0 {
				t.Fatal("unsafe/nonempty state reached issuer writes", err, writes)
			}
			if _, err = os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
				t.Fatal("preflight failure left issuer", err)
			}
			if mode == "occupied" {
				raw, err := os.ReadFile(filepath.Join(states[1], "existing-state"))
				if err != nil || string(raw) != "preserve" {
					t.Fatal("existing state changed", err)
				}
			}
		})
	}
}

func TestPairCompletionRequiresExactlyTwoNodes(t *testing.T) {
	dir, pair, _ := pairFixture(t)
	raw, err := os.ReadFile(filepath.Join(dir, "pair-public.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]json.RawMessage
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	for _, nodes := range [][]PairNode{{pair.Nodes[0]}, {pair.Nodes[0], pair.Nodes[1], pair.Nodes[0]}} {
		value["nodes"], err = json.Marshal(nodes)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "pair-public.json"), changed, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = load(pair.Nodes[0].Directory); err == nil {
			t.Fatal("missing or excess pair member accepted")
		}
	}
}

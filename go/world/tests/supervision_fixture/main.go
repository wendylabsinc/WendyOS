// supervision_fixture shortens generated lab supervisor credentials or revokes
// a generated supervisor or mission issuer in an explicit simulation-only mode.
// It never enrolls an identity, contacts a device, or edits its input files.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

const maxInputBytes = 1 << 20

type issuer struct {
	Domain      string                      `json:"domain"`
	PrivateKey  []byte                      `json:"private_key"`
	PublicKey   []byte                      `json:"public_key"`
	Revocations authority.SignedRevocations `json:"revocations"`
}
type options struct {
	issuer, credentials, out string
	ttl                      time.Duration
	revoke                   bool
	revokeMissionIssuer      bool
	ttlSet                   bool
}

func readPrivate(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("fixture input cannot be opened")
	}
	file := os.NewFile(uintptr(fd), "supervision-fixture-input")
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("fixture input must be an owner-only regular file")
	}
	b, err := io.ReadAll(io.LimitReader(file, maxInputBytes+1))
	if err != nil || len(b) > maxInputBytes {
		clear(b)
		return nil, errors.New("fixture input exceeds bound")
	}
	return b, nil
}
func consistentKey(key, public []byte) bool {
	return len(key) == ed25519.PrivateKeySize && len(public) == ed25519.PublicKeySize && bytes.Equal(ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]), key) && bytes.Equal(key[ed25519.SeedSize:], public)
}
func writeExclusive(path string, data []byte) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errors.New("new fixture output cannot be created")
	}
	file := os.NewFile(uintptr(fd), "supervision-fixture-output")
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(path)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return errors.New("fixture output write failed")
	}
	if err = file.Sync(); err != nil {
		return errors.New("fixture output sync failed")
	}
	if err = file.Close(); err != nil {
		return errors.New("fixture output close failed")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errors.New("fixture output directory unavailable")
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return errors.New("fixture output directory sync failed")
	}
	success = true
	return nil
}
func execute(o options, now time.Time) error {
	if o.issuer == "" || o.credentials == "" || o.out == "" || now.IsZero() || now.UnixMilli() <= 0 {
		return errors.New("explicit fixture inputs and output required")
	}
	if o.revoke && o.revokeMissionIssuer {
		return errors.New("revocation modes are mutually exclusive")
	}
	if o.revoke || o.revokeMissionIssuer {
		if o.ttl != 0 || o.ttlSet {
			return errors.New("revoke and ttl are mutually exclusive")
		}
	} else if o.ttl < time.Millisecond || o.ttl > time.Minute {
		return errors.New("fixture ttl must be 1ms through 1m")
	}
	issuerBytes, err := readPrivate(o.issuer)
	if err != nil {
		return err
	}
	defer clear(issuerBytes)
	credentialBytes, err := readPrivate(o.credentials)
	if err != nil {
		return err
	}
	defer clear(credentialBytes)
	var root issuer
	var credentials client.Config
	if authority.DecodeStrict(issuerBytes, &root) != nil || authority.DecodeStrict(credentialBytes, &credentials) != nil {
		return errors.New("invalid fixture schema")
	}
	defer clear(root.PrivateKey)
	defer clear(credentials.PrivateKey)
	g := credentials.Grant.Grant
	if root.Domain != "simulation" || g.Domain != "simulation" || g.Tenant != "lab" || !strings.HasPrefix(g.ID, "lab/") || !strings.HasPrefix(g.Principal, "lab/") || g.Kind != "operator" || !credentials.AllowInsecureSimulation || g.PublishAudience != "" || g.ExecuteAssignee != "" || g.TaskStatusAssignee != "" || g.TaskStatusAudience != "" {
		return errors.New("only generated lab supervision identities are supported")
	}
	required := map[string]bool{"supervise": false, "stop": false, "read": false}
	if o.revokeMissionIssuer {
		parts := strings.Split(g.ID, "/")
		if len(parts) != 3 || parts[0] != "lab" || parts[1] == "" || len(parts[1]) > 64 || parts[2] != "sim-operator" || g.Principal != "lab/sim-operator" || g.Site != "synthetic-office" || credentials.Audience != "node-1" || g.Source != (world.Source{Device: "sim-operator", App: "wendy-world", Service: "sim-operator", Stream: "state"}) {
			return errors.New("mission revocation requires the generated lab mission issuer")
		}
		required = map[string]bool{"mission": false, "stop": false, "read": false}
	}
	for _, scope := range g.Scopes {
		seen, ok := required[scope]
		if !ok || seen {
			return errors.New("grant scopes differ from the selected fixture mode")
		}
		required[scope] = true
	}
	for _, present := range required {
		if !present {
			return errors.New("grant scopes differ from the selected fixture mode")
		}
	}
	if !consistentKey(root.PrivateKey, root.PublicKey) || !consistentKey(credentials.PrivateKey, g.PublicKey) {
		return errors.New("fixture key and public identity do not match")
	}
	if _, err = client.New(credentials); err != nil {
		return errors.New("invalid simulation client profile")
	}
	verifier, err := authority.NewVerifier(authority.Config{RootPublicKey: ed25519.PublicKey(root.PublicKey), Tenant: g.Tenant, Site: g.Site, Domain: "simulation", Audience: credentials.Audience, Now: func() time.Time { return now }, Replay: authority.NewMemoryReplay(8), MaxGrantLifetime: time.Hour, MaxRevocationAge: time.Hour})
	if err != nil {
		return errors.New("invalid fixture authority scope")
	}
	if err = verifier.UpdateRevocations(root.Revocations); err != nil {
		return errors.New("fixture revocation snapshot invalid or stale")
	}
	for scope := range required {
		if _, err = verifier.VerifyGrant(credentials.Grant, scope); err != nil {
			return errors.New("original fixture grant is invalid, expired or revoked")
		}
	}
	var output any
	if o.revoke || o.revokeMissionIssuer {
		r := root.Revocations.Revocations
		if r.Revision == math.MaxUint64 {
			return errors.New("fixture revocation revision exhausted")
		}
		r.Revision++
		r.IssuedAtUnixMS = now.UnixMilli()
		r.Principals = append(append([]string(nil), r.Principals...), g.Principal)
		signed, err := authority.SignRevocations(r, ed25519.PrivateKey(root.PrivateKey))
		if err != nil {
			return errors.New("fixture revocation signing failed")
		}
		if err = verifier.ValidateRevocations(signed); err != nil {
			return errors.New("fixture revocation update invalid")
		}
		output = signed
	} else {
		nonce := make([]byte, 16)
		if _, err = rand.Read(nonce); err != nil {
			return errors.New("fixture nonce unavailable")
		}
		g.ID = g.ID + "/short-" + hex.EncodeToString(nonce)
		g.NotBeforeUnixMS = now.UnixMilli()
		g.NotAfterUnixMS = min(now.Add(o.ttl).UnixMilli(), g.NotAfterUnixMS)
		signed, err := authority.SignGrant(g, ed25519.PrivateKey(root.PrivateKey))
		if err != nil {
			return errors.New("fixture grant signing failed")
		}
		if _, err = verifier.VerifyGrant(signed, "supervise"); err != nil {
			return errors.New("fixture grant validation failed")
		}
		credentials.Grant = signed
		output = credentials
	}
	encoded, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return errors.New("fixture encoding failed")
	}
	defer clear(encoded)
	return writeExclusive(o.out, append(encoded, '\n'))
}
func main() {
	var o options
	flag.StringVar(&o.issuer, "issuer", "", "generated lab issuer file")
	flag.StringVar(&o.credentials, "credentials", "", "generated supervisor profile, or mission issuer in its explicit revocation mode")
	flag.StringVar(&o.out, "out", "", "new owner-only output file")
	flag.DurationVar(&o.ttl, "ttl", 0, "shorter grant lifetime, at most 1m")
	flag.BoolVar(&o.revoke, "revoke", false, "emit principal revocation revision plus one")
	flag.BoolVar(&o.revokeMissionIssuer, "revoke-mission-issuer", false, "revoke only the generated lab mission issuer; excludes revoke and ttl")
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "ttl" {
			o.ttlSet = true
		}
	})
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected fixture arguments")
		os.Exit(2)
	}
	if err := execute(o, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "supervision fixture:", err)
		os.Exit(1)
	}
}

// world-sign reads an explicitly selected enrolled operator credential once. It
// emits a public proof and never transports, installs or persists that proof.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/authenticatedtime"
	"github.com/wendylabsinc/wendy/go/world/production/operator"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type dependencies struct {
	acquire func(context.Context) (trustedtime.Provider, error)
	custody operator.Keychain
}

func runtimeDependencies() dependencies {
	return dependencies{custody: operator.SystemKeychain(), acquire: func(ctx context.Context) (trustedtime.Provider, error) {
		return authenticatedtime.Acquire(ctx, authenticatedtime.Config{Servers: authenticatedtime.WendyServers(), QueryTimeout: 5 * time.Second, MaxHoldover: 10 * time.Minute, DriftPPM: 100, MaxWidth: 5 * time.Second})
	}}
}

const usage = `world-sign describe --config FILE --context NAME --tenant UUID --principal SPIFFE
world-sign read --config FILE --context NAME --tenant UUID --principal SPIFFE --trust-roots PEM --time-source wendy --site SITE --audience SERVICE --policy-revision N --body-file FILE
world-sign read-policy --config FILE --context NAME --tenant UUID --principal SPIFFE --trust-roots PEM --time-source wendy --site SITE --audience SERVICE --policy-id ID --revision N --lifetime 5m
world-sign revocations --config FILE --context NAME --tenant UUID --principal SPIFFE --trust-roots PEM --time-source wendy --site SITE --audience SERVICE --revision N --lifetime 5m [--revoke-self] [--revoke-key] [--revoke-certificate] [--revoke-policy --policy-id ID]
`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, 30*time.Second)
	defer deadline()
	if err := run(ctx, os.Args[1:], os.Stdout, runtimeDependencies()); err != nil {
		fmt.Fprintln(os.Stderr, "world-sign:", err)
		os.Exit(1)
	}
}

var token = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func run(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(out, usage)
		return err
	}
	if len(args) == 0 {
		return errors.New("explicit subcommand required; use world-sign help")
	}
	command := args[0]
	if command != "describe" && command != "read" && command != "read-policy" && command != "revocations" {
		return errors.New("unknown subcommand; use world-sign help")
	}
	fs := flag.NewFlagSet("world-sign", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var selected operator.Selection
	fs.StringVar(&selected.ConfigPath, "config", "", "existing credential configuration")
	fs.StringVar(&selected.Context, "context", "", "explicit named context")
	fs.StringVar(&selected.Tenant, "tenant", "", "canonical tenant UUID")
	fs.StringVar(&selected.Principal, "principal", "", "canonical operator SPIFFE principal")
	var rootsFile, timeSource, site, audience, policyID, bodyFile string
	var revision uint64
	var lifetime time.Duration
	var self, key, cert, policy bool
	if command != "describe" {
		fs.StringVar(&rootsFile, "trust-roots", "", "commissioned public trust bundle")
		fs.StringVar(&timeSource, "time-source", "", "wendy pinned signed quorum")
		fs.StringVar(&site, "site", "", "explicit site")
		fs.StringVar(&audience, "audience", "", "explicit service audience")
		if command == "read" {
			fs.Uint64Var(&revision, "policy-revision", 0, "required policy revision")
			fs.StringVar(&bodyFile, "body-file", "", "exact read-query JSON body file")
		} else {
			fs.Uint64Var(&revision, "revision", 0, "monotonically increasing bundle revision")
			fs.DurationVar(&lifetime, "lifetime", 0, "explicit duration from 1s through 10m")
			fs.StringVar(&policyID, "policy-id", "", "explicit policy identifier")
			if command == "revocations" {
				fs.BoolVar(&self, "revoke-self", false, "revoke selected principal")
				fs.BoolVar(&key, "revoke-key", false, "revoke selected signing key")
				fs.BoolVar(&cert, "revoke-certificate", false, "revoke selected certificate")
				fs.BoolVar(&policy, "revoke-policy", false, "revoke explicit local policy ID")
			}
		}
	}
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
		return errors.New("invalid command arguments; use world-sign help")
	}
	if duplicateFlags(fs, args[1:]) {
		return errors.New("duplicate command flag")
	}
	if ctx == nil || ctx.Err() != nil {
		return errors.New("live signing context required")
	}
	if selected.ConfigPath == "" || selected.Context == "" || selected.Tenant == "" || selected.Principal == "" {
		return errors.New("config, context, tenant and principal must be explicit")
	}
	if command == "describe" {
		metadata, err := operator.Describe(selected)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(metadata)
	}
	if rootsFile == "" || timeSource != "wendy" || !token.MatchString(site) || audience == "" || len(audience) > 512 || revision == 0 {
		return errors.New("explicit trust, wendy time source, bounded target and positive revision required")
	}
	if command != "read" && (lifetime < time.Second || lifetime > 10*time.Minute) {
		return errors.New("explicit commissioning lifetime must be between 1s and 10m")
	}
	if command == "read-policy" && !token.MatchString(policyID) {
		return errors.New("bounded policy ID required")
	}
	if command == "revocations" && ((policy && !token.MatchString(policyID)) || (!policy && policyID != "")) {
		return errors.New("policy ID is required only with revoke-policy")
	}
	var body []byte
	if command == "read" {
		var err error
		body, err = operator.ReadBodyFile(bodyFile)
		if err != nil {
			return err
		}
		defer clear(body)
	}
	// Describe validates all identity/context fields without opening key custody.
	if _, err := operator.Describe(selected); err != nil {
		return err
	}
	roots, err := operator.LoadTrustRoots(rootsFile)
	if err != nil {
		return err
	}
	verifier, err := wendycert.New(wendycert.Config{Tenant: selected.Tenant, Roots: roots})
	if err != nil {
		return errors.New("commissioned trust configuration invalid")
	}
	if deps.acquire == nil {
		return errors.New("authenticated time acquisition unavailable")
	}
	clock, err := deps.acquire(ctx)
	if err != nil {
		return fmt.Errorf("authenticated time unavailable: %w", err)
	}
	signer := operator.Signer{Certificates: verifier, Clock: clock, Keychain: deps.custody}
	var proof string
	switch command {
	case "read":
		proof, err = signer.SignRead(ctx, selected, operator.ReadTarget{Site: site, Audience: audience, PolicyRevision: revision}, body)
	case "read-policy":
		proof, err = signer.SignReadPolicy(ctx, selected, operator.PolicyTarget{Site: site, Audience: audience, PolicyID: policyID, Revision: revision, Lifetime: lifetime})
	case "revocations":
		proof, err = signer.SignRevocations(ctx, selected, operator.RevocationTarget{Site: site, Audience: audience, PolicyID: policyID, Revision: revision, Lifetime: lifetime, RevokeSelf: self, RevokeKey: key, RevokeCertificate: cert, RevokePolicy: policy})
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, proof)
	return err
}

// Flag values are skipped so a path or audience beginning with '-' remains a value.
func duplicateFlags(fs *flag.FlagSet, args []string) bool {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		name, _, hasEquals := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if seen[name] {
			return true
		}
		seen[name] = true
		f := fs.Lookup(name)
		if f != nil && !hasEquals {
			b, ok := f.Value.(interface{ IsBoolFlag() bool })
			if !ok || !b.IsBoolFlag() {
				i++
			}
		}
	}
	return false
}

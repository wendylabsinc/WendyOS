package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func main() {
	addr := flag.String("addr", "", "mTLS agent address (host:port)")
	principal := flag.String("expected-principal", "", "trusted expected device SPIFFE principal")
	orgID := flag.Int("expected-org", 0, "trusted legacy device organization ID")
	assetID := flag.String("expected-asset", "", "trusted legacy device asset ID")
	flag.Parse()

	if *addr == "" {
		fmt.Fprintln(os.Stderr, "usage: test_mtls -addr <host:port> -expected-principal <spiffe URI> (or -expected-org <org> -expected-asset <asset>)")
		os.Exit(1)
	}

	expected := certs.WendyIdentity{OrgID: int32(*orgID), EntityType: certs.EntityAsset, EntityID: *assetID}
	if *principal != "" {
		var err error
		expected, err = certs.ParsePrincipal(*principal)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if expected.EntityType != certs.EntityAsset || expected.EntityID == "" || !expected.Scope().Known() {
		fmt.Fprintln(os.Stderr, "provide a trusted device identity with -expected-principal or -expected-org and -expected-asset")
		os.Exit(1)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if len(cfg.Auth) == 0 || len(cfg.Auth[0].Certificates) == 0 {
		fmt.Fprintf(os.Stderr, "no certs found\n")
		os.Exit(1)
	}
	cert := cfg.Auth[0].Certificates[0]
	fmt.Printf("Cert PEM len: %d, Chain len: %d\n", len(cert.PemCertificate), len(cert.PemCertificateChain))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpcclient.ConnectWithTLSExpecting(ctx, *addr, &cert, nil, &expected)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ConnectWithTLS: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "GetAgentVersion: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Version: %s\n", resp.GetVersion())
}

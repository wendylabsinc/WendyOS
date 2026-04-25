package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func main() {
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
	
	conn, err := grpcclient.ConnectWithTLS(ctx, "wendyos-dynamic-cosmos.local:50052", &cert)
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

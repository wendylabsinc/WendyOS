// wendy-cloud-mcp is the always-on, organization-scoped MCP and CLI gateway.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
	"github.com/wendylabsinc/wendy/go/internal/cloudmcp"
)

type machineConfig struct {
	Organization string `json:"organization"`
	Issuer       string `json:"issuer"`
	Subject      string `json:"subject"`
	KeyFile      string `json:"key_file"`
	KeyPEM       string `json:"key_pem"`
}
type configuration struct {
	Origin         string               `json:"origin"`
	CloudHTTP      string               `json:"cloud_http"`
	Listen         string               `json:"listen"`
	TLSCertificate string               `json:"tls_certificate"`
	TLSKey         string               `json:"tls_key"`
	TraceEndpoint  string               `json:"trace_endpoint,omitempty"`
	Services       browserauth.Settings `json:"services"`
	Machines       []machineConfig      `json:"machines"`
}

func run(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var cfg configuration
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return fmt.Errorf("invalid MCP configuration: %w", err)
	}
	if cfg.Listen == "" || cfg.TLSCertificate == "" || cfg.TLSKey == "" {
		return fmt.Errorf("listen and TLS certificate/key files are required")
	}
	if err := cfg.Services.Validate(); err != nil {
		return err
	}
	shutdownTelemetry, err := configureTelemetry(ctx, cfg.TraceEndpoint)
	if err != nil {
		return err
	}
	defer shutdownTelemetry()
	sessions := make(map[string]*browserauth.MachineSession, len(cfg.Machines))
	for _, machine := range cfg.Machines {
		if _, exists := sessions[machine.Organization]; exists {
			return fmt.Errorf("duplicate organization machine")
		}
		if (machine.KeyFile == "") == (machine.KeyPEM == "") {
			return fmt.Errorf("configure exactly one machine key source")
		}
		key := []byte(machine.KeyPEM)
		if machine.KeyFile != "" {
			var err error
			key, err = os.ReadFile(machine.KeyFile)
			if err != nil {
				return fmt.Errorf("reading machine key file: %w", err)
			}
		}
		session, err := browserauth.NewMachineSession(cfg.Services, machine.Issuer, machine.Organization, machine.Subject, string(key), nil)
		if err != nil {
			return err
		}
		sessions[machine.Organization] = session
	}
	backend, err := cloudmcp.NewCloudBackend(cfg.CloudHTTP, sessions)
	if err != nil {
		return err
	}
	handler, err := cloudmcp.New(cfg.Origin, backend, backend)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: cfg.Listen, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10,
		TLSConfig:   &tls.Config{MinVersion: tls.VersionTLS13},
		BaseContext: func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			handler.ServeHTTP(w, r)
		}),
	}
	go func() {
		<-ctx.Done()
		drain, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(drain)
	}()
	slog.Info("hosted MCP listening", "origin", cfg.Origin, "address", cfg.Listen)
	if err := server.ListenAndServeTLS(cfg.TLSCertificate, cfg.TLSKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
func main() {
	path := flag.String("config", "/etc/wendy-cloud-mcp/config.json", "deployment configuration and mounted secret paths")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *path); err != nil {
		slog.Error("hosted MCP stopped", "error", err)
		os.Exit(1)
	}
}

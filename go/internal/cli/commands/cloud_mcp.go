package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/hostedmcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func connectHostedMCP(ctx context.Context, selector string) (*grpcclient.AgentConnection, error) {
	return hostedmcp.Connect(ctx, selector, hostedMCPToken)
}
func hostedMCPToken(ctx context.Context, resource string) (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	for i := range cfg.Auth {
		auth := &cfg.Auth[i]
		if auth.OAuthResource == resource && auth.OAuthIssuer != "" {
			if err := ensureOAuthAccessToken(ctx, auth); err != nil {
				return "", err
			}
			if auth.OAuthResource != resource {
				return "", fmt.Errorf("MCP session audience changed")
			}
			return auth.APIKey, nil
		}
	}
	return "", fmt.Errorf("sign in with 'wendy auth login --issuer <realm-issuer> --resource %s'", resource)
}

func isHostedMCPSelector(selector string) bool { return strings.HasPrefix(selector, "mcp:") }

// Owner settings use the ordinary Cloud session, with a fresh DPoP proof.
func newCloudMCPCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mcp", Short: "Manage organization hosted MCP access", Hidden: true}
	for _, operation := range []string{"status", "enable", "disable"} {
		var organization, subject, origin string
		child := &cobra.Command{Use: operation, Short: operation + " organization hosted MCP", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				id, err := uuid.Parse(organization)
				if err != nil || id == uuid.Nil || id.String() != organization {
					return fmt.Errorf("--organization must be a canonical UUID")
				}
				u, err := url.Parse(origin)
				if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
					return fmt.Errorf("--cloud-http must be an HTTPS origin")
				}
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				var auth *config.AuthConfig
				for i := range cfg.Auth {
					candidate := &cfg.Auth[i]
					if candidate.CloudGRPC == u.Hostname()+":443" && candidate.OAuthIssuer != "" && !strings.Contains(candidate.OAuthResource, "/orgs/") {
						auth = candidate
						break
					}
				}
				if auth == nil {
					return fmt.Errorf("sign in to this Cloud deployment first")
				}
				if err := ensureOAuthAccessToken(cmd.Context(), auth); err != nil {
					return err
				}
				keyPEM, err := auth.OAuthDPoPKey()
				if err != nil {
					return err
				}
				key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
				if err != nil {
					return err
				}
				endpoint := origin + "/v1/hosted-mcp/orgs/" + organization + "/settings"
				proof, err := cloudrequest.NewDPoPAccessProof(key, http.MethodPost, endpoint, auth.APIKey)
				if err != nil {
					return err
				}
				body := map[string]any{}
				if operation != "status" {
					body["enabled"] = operation == "enable"
					if subject != "" {
						body["service_subject"] = subject
					}
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					return err
				}
				request, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, endpoint, bytes.NewReader(encoded))
				if err != nil {
					return err
				}
				request.Header.Set("Authorization", "DPoP "+auth.APIKey)
				request.Header.Set("DPoP", proof)
				request.Header.Set("Content-Type", "application/json")
				client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				response, err := client.Do(request)
				if err != nil {
					return err
				}
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK {
					return fmt.Errorf("Cloud refused MCP settings (HTTP %d); organization ownership is required", response.StatusCode)
				}
				result, err := io.ReadAll(io.LimitReader(response.Body, 65537))
				if err != nil {
					return err
				}
				if len(result) > 65536 || !json.Valid(result) {
					return fmt.Errorf("invalid Cloud response")
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(result))
				return nil
			}}
		child.Flags().StringVar(&organization, "organization", "", "Organization UUID")
		child.Flags().StringVar(&subject, "service-account", "", "Provisioned machine-account subject (enable only)")
		child.Flags().StringVar(&origin, "cloud-http", "https://api.dev.wendy.sh", "Cloud HTTP API origin")
		cmd.AddCommand(child)
	}
	cmd.AddCommand(newHostedMCPTunnelCmd())
	return cmd
}

func newHostedMCPTunnelCmd() *cobra.Command {
	var selector, service, listen string
	cmd := &cobra.Command{Use: "tunnel", Short: "Forward a loopback port to a hosted device service", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if _, err := hostedmcp.Parse(selector); err != nil {
			return err
		}
		if service != "ssh" && service != "wendy-registry" && service != "wendy-registry-darwin" {
			return fmt.Errorf("unsupported catalog service")
		}
		host, _, err := net.SplitHostPort(listen)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("--listen must bind a loopback address")
		}
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			return err
		}
		defer listener.Close()
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		go func() { <-ctx.Done(); listener.Close() }()
		fmt.Fprintln(cmd.OutOrStdout(), "Forwarding", listener.Addr(), "to", service)
		slots := make(chan struct{}, 16)
		var group sync.WaitGroup
		defer group.Wait()
		defer cancel()
		for {
			local, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			select {
			case slots <- struct{}{}:
			default:
				local.Close()
				continue
			}
			group.Add(1)
			go func() {
				defer group.Done()
				defer func() { <-slots }()
				defer local.Close()
				remote, err := hostedmcp.DialService(ctx, selector, service, hostedMCPToken)
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "Device tunnel unavailable:", err)
					return
				}
				defer remote.Close()
				finished := make(chan struct{})
				go func() {
					select {
					case <-ctx.Done():
						local.Close()
						remote.Close()
					case <-finished:
					}
				}()
				defer close(finished)
				go func() { _, _ = io.Copy(remote, local); remote.Close() }()
				_, _ = io.Copy(local, remote)
			}()
		}
	}}
	cmd.Flags().StringVar(&selector, "device", "", "mcp://host/orgs/<UUID>/devices/<UUID>")
	cmd.Flags().StringVar(&service, "service", "ssh", "Cloud service: ssh, wendy-registry, or wendy-registry-darwin")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:2222", "Local loopback listen address")
	return cmd
}

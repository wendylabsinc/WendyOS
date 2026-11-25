// Command profilecheck verifies client-created read requests offline using the
// commissioner's actual public root. It does not start a gateway or contact one.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type publicConfig struct {
	Version     int                         `json:"version"`
	Tenant      string                      `json:"tenant"`
	Site        string                      `json:"site"`
	Audience    string                      `json:"audience"`
	Root        ed25519.PublicKey           `json:"root_public_key"`
	Revocations authority.SignedRevocations `json:"revocations"`
}

func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func run() error {
	if len(os.Args) != 4 {
		return fmt.Errorf("public config and two signed request paths required")
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		return err
	}
	var public publicConfig
	if err = authority.DecodeStrictLimit(raw, &public, 64<<10); err != nil {
		return err
	}
	if public.Version != 1 || public.Audience != "sharedinspection-native-viewer" {
		return fmt.Errorf("wrong viewer public config")
	}
	results := []map[string]any{}
	for i, name := range []string{"apple", "android"} {
		requestRaw, err := os.ReadFile(os.Args[i+2])
		if err != nil {
			return err
		}
		r, err := authority.DecodeRequest(requestRaw)
		if err != nil {
			return err
		}
		if r.Operation != "read" || len(r.Grant.Grant.Scopes) != 2 || r.Grant.Grant.Scopes[0] != "read" || r.Grant.Grant.Scopes[1] != "evidence-read" {
			return fmt.Errorf("unexpected request scope")
		}
		// Explicit historical verification at the signed request's original time.
		// This is not a current-freshness claim for a later replay of the check.
		now := time.UnixMilli(r.IssuedAtUnixMS + 1)
		replay := authority.NewMemoryReplay(100)
		defer replay.Close()
		verifier, err := authority.NewVerifier(authority.Config{RootPublicKey: public.Root, Tenant: public.Tenant,
			Site: public.Site, Domain: "simulation", Audience: public.Audience, Now: func() time.Time { return now }, Replay: replay})
		if err != nil {
			return err
		}
		if err = verifier.UpdateRevocations(public.Revocations); err != nil {
			return err
		}
		calls := 0
		handler, err := node.NewComponentReadHandler(node.ComponentReadConfig{Tenant: public.Tenant, Site: public.Site, Audience: public.Audience, MaxRecords: 16}, verifier,
			func(context.Context) ([]world.Component, error) { calls++; return []world.Component{}, nil })
		if err != nil {
			return err
		}
		req := httptest.NewRequest(http.MethodGet, "http://fixture.invalid/v1/components", nil)
		req.Header.Set("X-World-Authorization", base64.StdEncoding.EncodeToString(requestRaw))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK || calls != 1 {
			return fmt.Errorf("%s authorized query refused with HTTP %d", name, response.Code)
		}
		duplicate := httptest.NewRecorder()
		handler.ServeHTTP(duplicate, req)
		if duplicate.Code != http.StatusUnauthorized || calls != 1 {
			return fmt.Errorf("%s replay accepted", name)
		}
		results = append(results, map[string]any{"client": name, "request_sha256": hash(requestRaw), "request_bytes": len(requestRaw),
			"signature_verified": true, "component_handler_status": response.Code, "replay_status": duplicate.Code, "read_callbacks": calls,
			"verification_unix_ms": now.UnixMilli()})
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"version": 1, "status": "passed", "public_config_sha256": hash(raw), "clients": results,
		"scope": "Offline original-time authority verification and actual in-process component HTTP handler; no listener, Agent, native UI or deployment"})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

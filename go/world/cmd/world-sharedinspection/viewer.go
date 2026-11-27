package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const viewerAudience = "sharedinspection-native-viewer"

type viewerPublic struct {
	Version       int                         `json:"version"`
	Tenant        string                      `json:"tenant"`
	Site          string                      `json:"site"`
	Audience      string                      `json:"audience"`
	RootPublicKey ed25519.PublicKey           `json:"root_public_key"`
	Revocations   authority.SignedRevocations `json:"revocations"`
}

type viewerProfile struct {
	Name        string        `json:"name"`
	ReadMode    string        `json:"read_mode"`
	Credentials client.Config `json:"credentials"`
}

// The optional viewer uses the existing read protocol with its own root and
// reader key. None of those credentials can address the mission authority.
func commissionViewer(input commissionInput, now time.Time) (viewerPublic, viewerProfile, error) {
	var public viewerPublic
	var profile viewerProfile
	root, issuer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return public, profile, err
	}
	reader, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return public, profile, err
	}
	start, end := now.Add(-time.Second).UnixMilli(), now.Add(59*time.Minute).UnixMilli()
	grant, err := authority.SignGrant(authority.Grant{
		Version: 1, ID: "sharedinspection/" + input.RunID + "/viewer-reader",
		Tenant: input.Tenant, Site: input.Site, Domain: "simulation",
		Principal: "sharedinspection/viewer-reader", Kind: "operator",
		Source:    world.Source{Device: input.Device, App: "wendy-world-viewer", Service: "viewer-reader", Stream: "components"},
		PublicKey: reader, Scopes: []string{"read", "evidence-read"}, NotBeforeUnixMS: start, NotAfterUnixMS: end,
	}, issuer)
	if err != nil {
		return public, profile, err
	}
	revocations, err := authority.SignRevocations(authority.Revocations{
		Version: 1, Tenant: input.Tenant, Site: input.Site, Domain: "simulation", Revision: 1,
		IssuedAtUnixMS: start, ExpiresAtUnixMS: end,
	}, issuer)
	if err != nil {
		return public, profile, err
	}
	public = viewerPublic{Version: 1, Tenant: input.Tenant, Site: input.Site, Audience: viewerAudience,
		RootPublicKey: root, Revocations: revocations}
	profile = viewerProfile{Name: "Shared-office native observations", ReadMode: "native_inference",
		Credentials: client.Config{URL: input.ViewerURL, Audience: viewerAudience, Grant: grant,
			PrivateKey: private, AllowInsecureSimulation: true}}
	// writeNew uses this exact indented encoding plus one newline. Keep the
	// complete profile inside the existing Android 64 KiB import limit.
	raw, err := json.MarshalIndent(profile, "", "  ")
	if err != nil || len(raw)+1 > 64<<10 {
		return viewerPublic{}, viewerProfile{}, errors.New("viewer profile exceeds client bound")
	}
	return public, profile, nil
}

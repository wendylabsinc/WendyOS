package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type fleetDevice struct {
	Asset string `json:"asset"`
	Host  string `json:"host"`
}

// Only modified assets enter the lease. Original tags are evidence, not a
// snapshot to replay: restoration removes only our unique tag from current tags.
type savedAsset struct {
	ID       string   `json:"id"`
	Site     string   `json:"site"`
	SiteName string   `json:"siteName"`
	Tags     []string `json:"tags"`
	Restored bool     `json:"restored"`
}
type fleetLease struct {
	App    string       `json:"app"`
	Site   string       `json:"site"`
	Assets []savedAsset `json:"assets"`
}

func saveLease(l fleetLease) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(".", ".fleet-lease-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, "fleet-lease.json"); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir("fleet-lease.json"))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func fleet(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig, o options) (any, error) {
	if os.Getenv("WENDY_E2E_APPS_FLEET") != "true" {
		return nil, atFixtureStage("fleet_opt_in", errors.New("dedicated fleet opt-in required"))
	}
	raw, err := os.ReadFile(os.Getenv("WENDY_E2E_APPS_FLEET_PATH"))
	if err != nil {
		return nil, atFixtureStage("fleet_manifest", errors.New("fleet fixture file required"))
	}
	var devices []fleetDevice
	if json.Unmarshal(raw, &devices) != nil || len(devices) < 3 {
		return nil, errors.New("three declared fleet devices required")
	}
	allowed := map[string]bool{}
	for _, d := range devices {
		if d.Asset == "" || d.Host == "" || allowed[d.Asset] {
			return nil, errors.New("invalid fleet declaration")
		}
		allowed[d.Asset] = true
	}
	if !allowed[o.asset] {
		return nil, errors.New("asset outside declared dedicated fleet")
	}
	ac := pb.NewAssetServiceClient(c)
	read := func(id string) (*pb.Asset, error) {
		if !allowed[id] {
			return nil, errors.New("asset outside declared dedicated fleet")
		}
		v, e := ac.GetAsset(ctx, &pb.GetAssetRequest{Id: id})
		if e != nil {
			return nil, e
		}
		if v.OrganizationId != o.tenant {
			return nil, errors.New("asset tenant mismatch")
		}
		return v, nil
	}
	write := func(req *pb.UpdateAssetRequest) error {
		err := cloudrequest.Invoke(ctx, c, a, pb.AssetService_UpdateAsset_FullMethodName, req, new(pb.Asset))
		if err != nil {
			return atFixtureStage("fleet_update_asset", err)
		}
		return nil
	}
	if o.operation == "fleet-state" {
		rows := make([]map[string]any, 0, len(devices))
		for _, d := range devices {
			asset, err := read(d.Asset)
			if err != nil {
				return nil, err
			}
			rows = append(rows, map[string]any{"id": asset.Id, "name": asset.Name, "site": asset.GetSite(), "siteId": asset.GetSiteId(), "tags": append([]string{}, asset.Tags...)})
		}
		return rows, nil
	}
	var l fleetLease
	if o.operation == "fleet-prepare" {
		if _, e := os.Stat("fleet-lease.json"); !os.IsNotExist(e) {
			return nil, atFixtureStage("fleet_existing_lease", errors.New("existing lease must be restored first"))
		}
		// Verify all declarations read-only; C never enters the mutation ledger.
		for _, d := range devices {
			if _, err = read(d.Asset); err != nil {
				return nil, err
			}
		}
		l.App = o.app
		if err = saveLease(l); err != nil {
			return nil, err
		}
		site := new(pb.Site)
		if err = cloudrequest.Invoke(ctx, c, a, pb.SiteService_CreateSite_FullMethodName, &pb.CreateSiteRequest{OrganizationId: o.tenant, Name: o.app, Description: "Owned E2E site"}, site); err != nil {
			return nil, atFixtureStage("fleet_create_site", err)
		}
		l.Site = site.Id
		if err = saveLease(l); err != nil {
			return nil, err
		}
	} else {
		raw, err = os.ReadFile("fleet-lease.json")
		if err != nil || json.Unmarshal(raw, &l) != nil || l.App != o.app {
			return nil, errors.New("owned lease required")
		}
	}
	if o.operation == "fleet-prepare" || o.operation == "fleet-join" {
		id := o.asset
		// Record ownership before attempting either update, including failure paths.
		if slices.ContainsFunc(l.Assets, func(v savedAsset) bool { return v.ID == id }) {
			return nil, errors.New("asset already leased")
		}
		current, e := read(id)
		if e != nil {
			return nil, e
		}
		l.Assets = append(l.Assets, savedAsset{ID: id, Site: current.GetSiteId(), SiteName: current.GetSite(), Tags: append([]string{}, current.Tags...)})
		if err = saveLease(l); err != nil {
			return nil, err
		}
		tags := append([]string{}, current.Tags...)
		if !slices.Contains(tags, o.app) {
			tags = append(tags, o.app)
		}
		// UpdateAsset.site takes a site name, while SiteService uses UUIDs.
		if err = write(&pb.UpdateAssetRequest{Id: id, Site: proto.String(o.app), Tags: tags, UpdateTags: true}); err != nil {
			return nil, err
		}
	} else {
		// Recover creation that succeeded immediately before a failed lease save.
		if l.Site == "" {
			sites, e := pb.NewSiteServiceClient(c).ListSites(ctx, &pb.ListSitesRequest{OrganizationId: o.tenant})
			if e != nil {
				return nil, e
			}
			for _, s := range sites.Sites {
				if s.Name == o.app {
					if l.Site != "" {
						return nil, errors.New("ambiguous owned site")
					}
					l.Site = s.Id
				}
			}
			if err = saveLease(l); err != nil {
				return nil, err
			}
		}
		for i := range l.Assets {
			saved := &l.Assets[i]
			if saved.Restored {
				continue
			}
			current, e := read(saved.ID)
			if e != nil {
				return nil, e
			}
			req := &pb.UpdateAssetRequest{Id: saved.ID}
			changed := false
			// Preserve an external site move. Restore only a site still owned by us.
			if l.Site != "" && current.GetSiteId() == l.Site {
				if saved.SiteName == "" && saved.Site != "" {
					sites, e := pb.NewSiteServiceClient(c).ListSites(ctx, &pb.ListSitesRequest{OrganizationId: o.tenant})
					if e != nil {
						return nil, e
					}
					for _, site := range sites.Sites {
						if site.Id == saved.Site {
							saved.SiteName = site.Name
						}
					}
					if saved.SiteName == "" {
						return nil, errors.New("original site name unavailable; retain lease")
					}
				}
				req.Site = proto.String(saved.SiteName)
				changed = true
			}
			if !slices.Contains(saved.Tags, o.app) && slices.Contains(current.Tags, o.app) {
				req.Tags = slices.DeleteFunc(append([]string{}, current.Tags...), func(t string) bool { return t == o.app })
				req.UpdateTags = true
				changed = true
			}
			if changed {
				if err = write(req); err != nil {
					return nil, err
				}
			}
			saved.Restored = true
			if err = saveLease(l); err != nil {
				return nil, err
			}
		}
		if l.Site != "" {
			sites, e := pb.NewSiteServiceClient(c).ListSites(ctx, &pb.ListSitesRequest{OrganizationId: o.tenant})
			if e != nil {
				return nil, e
			}
			for _, s := range sites.Sites {
				if s.Id == l.Site && s.DeviceCount != 0 {
					return nil, errors.New("owned site gained other members; retain lease for inspection")
				}
			}
			err = cloudrequest.Invoke(ctx, c, a, pb.SiteService_DeleteSite_FullMethodName, &pb.DeleteSiteRequest{OrganizationId: o.tenant, SiteId: l.Site}, new(pb.DeleteSiteResponse))
			if err != nil && status.Code(err) != codes.NotFound {
				return nil, atFixtureStage("fleet_delete_site", err)
			}
		}
		if err = os.Remove("fleet-lease.json"); err != nil {
			return nil, err
		}
	}
	return map[string]any{"site": l.Site, "tag": o.app}, nil
}

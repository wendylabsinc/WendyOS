package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/packagecache"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
)

// Uniform binary metadata lets identical bytes be shared across package roles.
const packageContentType = "application/octet-stream"

func packageClient(path string) (client.Config, *client.Client, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return client.Config{}, nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return client.Config{}, nil, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		return client.Config{}, nil, errors.New("package credentials must be a private regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	if err != nil {
		return client.Config{}, nil, err
	}
	var cfg client.Config
	if err = authority.DecodeStrict(raw, &cfg); err != nil {
		return cfg, nil, err
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return cfg, nil, errors.New("package commands currently support simulation only")
	}
	c, err := client.New(cfg)
	return cfg, c, err
}

func packageSite(ctx context.Context, c *client.Client, v *authority.Verifier, digest string) (siteFetch, error) {
	var raw json.RawMessage
	if err := c.Do(ctx, "read", http.MethodGet, "/v1/sites", nil, &raw); err != nil {
		return siteFetch{}, err
	}
	var reply siteReply
	if err := authority.DecodeStrictLimit(raw, &reply, geoSiteLimit); err != nil {
		return siteFetch{}, err
	}
	if reply.Publications == nil || len(reply.Publications) > 10000 {
		return siteFetch{}, errors.New("invalid site publication list")
	}
	var selected *siteFetch
	for _, record := range reply.Publications {
		publication, err := verifiedSiteRecord(record, v, time.Now())
		if err != nil {
			return siteFetch{}, fmt.Errorf("site origin rejected: %w", err)
		}
		if publication.SHA256 != digest {
			continue
		}
		if selected != nil && !bytes.Equal(selected.Publication.Document, publication.Document) {
			return siteFetch{}, errors.New("conflicting publication digest")
		}
		if selected == nil {
			selected = &siteFetch{Publication: publication, SignedRecord: record, Verified: true}
		}
	}
	if selected == nil {
		return siteFetch{}, errors.New("requested site publication is unavailable")
	}
	return *selected, nil
}

// Existing object ownership and retention are unchanged. A read verifies every
// byte before another writer's allocation can satisfy publication.
func uploadPackageBlob(ctx context.Context, c *client.Client, data []byte) (time.Time, error) {
	out, uploadErr := c.UploadEvidence(ctx, data, packageContentType, time.Time{})
	if uploadErr == nil {
		return time.UnixMilli(out.RetainUntilUnixMS), nil
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	existing, meta, err := c.DownloadEvidence(ctx, digest, int64(len(data)))
	if err != nil {
		return time.Time{}, uploadErr
	}
	if meta.ContentType != packageContentType || meta.Size != int64(len(data)) || !bytes.Equal(existing, data) {
		return time.Time{}, errors.New("existing package blob differs")
	}
	return time.UnixMilli(meta.RetainUntilUnixMS), nil
}
func downloadPackageBlob(ctx context.Context, c *client.Client, digest string, max int64) ([]byte, error) {
	raw, meta, err := c.DownloadEvidence(ctx, digest, max)
	if err != nil {
		return nil, err
	}
	if meta.ContentType != packageContentType {
		return nil, errors.New("package blob has incompatible content type")
	}
	return raw, nil
}
func bundleSize(b worldpackage.Bundle) int64 {
	total := int64(len(b.Root))
	for _, raw := range b.Files {
		total += int64(len(raw))
	}
	return total
}

func packageBinding(publication *sitepub.Publication, manifest worldpackage.Manifest) error {
	if publication.PackageSHA256 != manifest.SHA256 || publication.Site != manifest.Site {
		return errors.New("site document does not bind this package")
	}
	var cfg geography.SiteConfig
	if err := json.Unmarshal(publication.Document, &cfg); err != nil {
		return err
	}
	if cfg.Root.Revision == manifest.FrameRevision {
		return nil
	}
	for _, frame := range cfg.Frames {
		if frame.Ref.Revision == manifest.FrameRevision {
			return nil
		}
	}
	return errors.New("package frame revision is absent from signed site")
}

// Refresh only the signed revocation document. The verifier retains its original
// pinned root, scope and monotonic revocation checkpoint; a replacement trust file
// cannot rotate that root or roll the accepted revision backwards.
func refreshPackageRevocations(path string, verifier *authority.Verifier) error {
	raw, err := readGeoFile(path, geoSiteLimit)
	if err != nil {
		return err
	}
	var trust siteTrust
	if err = authority.DecodeStrictLimit(raw, &trust, geoSiteLimit); err != nil {
		return err
	}
	return verifier.UpdateRevocations(trust.Revocations)
}

func (inv *invocation) runPackage(args []string, output io.Writer) error {
	if len(args) > 0 && args[0] == "cache" {
		return inv.runPackageCache(args[1:], output)
	}
	if len(args) == 0 || (args[0] != "publish" && args[0] != "fetch") {
		return errors.New("usage: world package publish --credentials FILE --site FILE --package PATH/world.json --run-id RUN [--ttl 30m] --json; world package fetch --credentials FILE --trust FILE --sha256 SITE_DIGEST --out NEWDIR [--blob-credentials FILE] --json")
	}
	f := flag.NewFlagSet("package "+args[0], flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "site operator or reader credentials")
	document := f.String("site", "", "geographic site document")
	packagePath := f.String("package", "", "local package root JSON")
	runID := f.String("run-id", "", "simulation run")
	ttl := f.Duration("ttl", 30*time.Minute, "site publication lifetime")
	trust := f.String("trust", "", "pinned root and current signed revocations")
	digest := f.String("sha256", "", "exact signed site publication digest")
	destination := f.String("out", "", "new package directory")
	blobCredentials := f.String("blob-credentials", "", "explicit blob endpoint credentials; default site endpoint")
	cacheDirectory := f.String("cache-dir", "", "private persistent download cache; resume interrupted fetches")
	cacheBytes := f.Int64("cache-bytes", packagecache.DefaultMaxBytes, "cache reserved-byte quota, fixed when created")
	jsonOutput := f.Bool("json", true, "emit JSON")
	if err := inv.parseFlags(f, args[1:]); err != nil {
		return err
	}
	if *credentials == "" || f.NArg() != 0 || !*jsonOutput {
		return errors.New("package commands require credentials and JSON output without positional arguments")
	}
	explicitTTL := false
	f.Visit(func(x *flag.Flag) {
		if x.Name == "ttl" {
			explicitTTL = true
		}
	})
	if args[0] == "publish" {
		if *document == "" || *packagePath == "" || *runID == "" || *trust != "" || *digest != "" || *destination != "" || *blobCredentials != "" || *cacheDirectory != "" || *ttl < time.Millisecond || *ttl > authority.DefaultRecordLifetime {
			return errors.New("publish requires site, package and run-id; TTL in [1ms,24h]; no fetch flags")
		}
	} else if *trust == "" || !sensing.DigestValid(*digest) || *destination == "" || *document != "" || *packagePath != "" || *runID != "" || explicitTTL {
		return errors.New("fetch requires trust, exact site digest and new output directory; no publication flags")
	}
	if *cacheDirectory == "" && *cacheBytes != packagecache.DefaultMaxBytes {
		return errors.New("cache-bytes requires cache-dir")
	}
	cfg, c, err := packageClient(*credentials)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 2*time.Minute)
	defer cancel()
	if args[0] == "publish" {
		g := cfg.Grant.Grant
		if g.Kind != "operator" || !siteScope(g.Scopes, "publish") || !siteScope(g.Scopes, "site-publish") || !siteScope(g.Scopes, "evidence-write") || !siteScope(g.Scopes, "evidence-read") {
			return errors.New("package publishing requires operator publish, site-publish and evidence read/write grants")
		}
		raw, err := readGeoFile(*document, geoSiteLimit)
		if err != nil {
			return err
		}
		bundle, err := worldpackage.Load(*packagePath)
		if err != nil {
			return err
		}
		config := sitepub.Config{Tenant: g.Tenant, Site: g.Site, RunID: *runID, Source: g.Source, Epoch: 1, Sequence: 1, Now: time.Now(), TTL: *ttl}
		component, err := sitepub.Build(raw, config)
		if err != nil {
			return err
		}
		publication, err := sitepub.Validate(component)
		if err != nil {
			return err
		}
		if err = packageBinding(publication, bundle.Manifest); err != nil {
			return err
		}
		// No site is announced until every byte has reached its evidence endpoint.
		retention, err := uploadPackageBlob(ctx, c, bundle.Root)
		if err != nil {
			return err
		}
		uploaded := map[string]bool{bundle.Manifest.SHA256: true}
		for _, file := range bundle.Manifest.Files {
			if uploaded[file.SHA256] {
				continue
			}
			until, err := uploadPackageBlob(ctx, c, bundle.Files[file.Path])
			if err != nil {
				return fmt.Errorf("upload %s: %w", file.Path, err)
			}
			if until.Before(retention) {
				retention = until
			}
			uploaded[file.SHA256] = true
		}
		config.Epoch, err = c.NextEpoch(ctx)
		if err != nil {
			return err
		}
		config.Now = time.Now()
		config.TTL = min(*ttl, retention.Sub(config.Now))
		if config.TTL < time.Millisecond {
			return errors.New("package evidence expired before publication")
		}
		component, err = sitepub.Build(raw, config)
		if err != nil {
			return err
		}
		if err = c.PublishPackage(ctx, component); err != nil {
			return err
		}
		publication, err = sitepub.Validate(component)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(struct {
			Publication   *sitepub.Publication `json:"publication"`
			PackageSHA256 string               `json:"package_sha256"`
			Files         int                  `json:"files"`
			Bytes         int64                `json:"bytes"`
			Epoch         uint64               `json:"epoch"`
			Published     bool                 `json:"published"`
		}{publication, bundle.Manifest.SHA256, len(bundle.Files) + 1, bundleSize(bundle), config.Epoch, true})
	}
	if _, err = os.Lstat(*destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("package destination already exists")
		}
		return err
	}
	verifier, err := siteVerifier(*trust, cfg)
	if err != nil {
		return err
	}
	selected, err := packageSite(ctx, c, verifier, *digest)
	if err != nil {
		return err
	}
	blobs := c
	blobConfig := cfg
	if *blobCredentials != "" {
		otherConfig, blobClient, err := packageClient(*blobCredentials)
		if err != nil {
			return err
		}
		a, b := cfg.Grant.Grant, otherConfig.Grant.Grant
		if a.Tenant != b.Tenant || a.Site != b.Site || a.Domain != b.Domain {
			return errors.New("blob endpoint credentials are outside selected site scope")
		}
		blobs = blobClient
		blobConfig = otherConfig
	}
	var cache *packagecache.Cache
	var cacheResult *packageCacheResult
	if *cacheDirectory != "" {
		grant := blobConfig.Grant.Grant
		cache, err = packagecache.Open(*cacheDirectory, packagecache.Config{Scope: packagecache.Scope{
			Tenant: grant.Tenant, Site: grant.Site, Domain: grant.Domain, Audience: blobConfig.Audience, Endpoint: blobConfig.URL,
		}, MaxBytes: *cacheBytes})
		if err != nil {
			return err
		}
		defer cache.Close()
		cacheResult = &packageCacheResult{}
	}
	download := func(hash string, limit int64) ([]byte, error) {
		if cache == nil {
			return downloadPackageBlob(ctx, blobs, hash, limit)
		}
		data, progress, err := cache.Fetch(ctx, hash, limit, blobs.GetEvidence)
		cacheResult.ResumedBytes += progress.ResumedBytes
		cacheResult.DownloadedBytes += progress.DownloadedBytes
		if progress.CacheHit {
			cacheResult.Hits++
		}
		return data, err
	}
	raw, err := download(selected.Publication.PackageSHA256, worldpackage.MaxRootBytes)
	if err != nil {
		return err
	}
	manifest, err := worldpackage.Inspect(raw)
	if err != nil {
		return err
	}
	if err = packageBinding(selected.Publication, manifest); err != nil {
		return err
	}
	files := map[string][]byte{}
	total := int64(len(raw))
	for _, file := range manifest.Files {
		limit := int64(worldpackage.MaxFileBytes)
		if file.Size > 0 {
			limit = file.Size
		}
		if limit > worldpackage.MaxTotalBytes-total {
			limit = worldpackage.MaxTotalBytes - total
		}
		if limit < 1 {
			return errors.New("package aggregate size exceeded")
		}
		data, err := download(file.SHA256, limit)
		if err != nil {
			return fmt.Errorf("download %s: %w", file.Path, err)
		}
		total += int64(len(data))
		files[file.Path] = data
	}
	bundle, err := worldpackage.Verify(raw, files)
	if err != nil {
		return err
	}
	// Recheck document and origin after transfer. A valid earlier response cannot
	// authorize installing a package after its publication or trust has expired.
	if err = refreshPackageRevocations(*trust, verifier); err != nil {
		return err
	}
	if _, err = verifiedSiteRecord(selected.SignedRecord, verifier, time.Now()); err != nil {
		return err
	}
	if _, err = geography.DecodeSite(selected.Publication.Document); err != nil {
		return err
	}
	if err = worldpackage.Install(bundle, *destination); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		SitePublicationSHA256 string              `json:"site_publication_sha256"`
		PackageSHA256         string              `json:"package_sha256"`
		Files                 int                 `json:"files"`
		Bytes                 int64               `json:"bytes"`
		Path                  string              `json:"path"`
		Verified              bool                `json:"verified"`
		Installed             bool                `json:"installed"`
		Cache                 *packageCacheResult `json:"cache,omitempty"`
	}{*digest, bundle.Manifest.SHA256, len(bundle.Files) + 1, bundleSize(bundle), *destination, true, true, cacheResult})
}

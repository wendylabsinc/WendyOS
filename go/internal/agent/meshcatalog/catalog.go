package meshcatalog

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

type Receipt struct {
	Key        Key       `json:"key"`
	Generation uint64    `json:"generation"`
	Hash       string    `json:"hash"`
	Until      time.Time `json:"until"`
}

type storedRecord struct {
	record Record
	wire   SignedRecord
	hash   string
}

// Catalog is scoped to one authenticated org and named mesh. Persistence must
// durably commit generation receipts before Accept makes a record visible.
// This prevents a restart from resurrecting a removed or superseded service.
type Catalog struct {
	mu        sync.Mutex
	mutation  sync.Mutex
	mesh      string
	org       int32
	asset     int32
	limit     int
	creds     *localmesh.Credentials
	cache     *localmesh.IdentityCache
	authorize AuthorizePublication
	persist   func([]Receipt) error
	entries   map[Key]storedRecord
	history   map[Key]Receipt
}

func NewCatalog(mesh string, org, asset int32, limit int, creds *localmesh.Credentials,
	cache *localmesh.IdentityCache, authorize AuthorizePublication,
	receipts []Receipt, persist func([]Receipt) error, now time.Time) (*Catalog, error) {
	if !labelPattern.MatchString(mesh) || org <= 0 || asset <= 0 || asset > 65534 ||
		limit < 1 || limit > 8192 || creds == nil || creds.Org != org || creds.Asset != asset ||
		cache == nil || authorize == nil || persist == nil {
		return nil, errors.New("invalid mesh catalog configuration")
	}
	if len(receipts) > limit {
		return nil, errors.New("too many persisted mesh service receipts")
	}
	history := make(map[Key]Receipt, len(receipts))
	for _, r := range receipts {
		if r.Key.Mesh != mesh || r.Key.Org != org || r.Key.Asset <= 0 || r.Key.Asset > 65534 ||
			!labelPattern.MatchString(r.Key.ServiceID) || r.Generation == 0 || len(r.Hash) != 64 {
			return nil, errors.New("invalid persisted mesh service receipt")
		}
		if _, err := hex.DecodeString(r.Hash); err != nil {
			return nil, errors.New("invalid persisted mesh service receipt hash")
		}
		if err := (Record{Version: 1, Key: r.Key, Generation: r.Generation,
			Issued: now.UnixMilli(), Expires: now.Add(time.Second).UnixMilli(), Withdraw: true}).Validate(now); err != nil {
			return nil, fmt.Errorf("invalid persisted mesh service owner: %w", err)
		}
		if _, found := history[r.Key]; found {
			return nil, errors.New("duplicate persisted mesh service receipt")
		}
		// Own receipts are a durable generation high-water, even after the
		// signed record and peers' replay windows expire. An app can advertise
		// the same stable service ID again after a long mesh disconnect.
		if r.Key.Asset == asset || r.Until.After(now) {
			history[r.Key] = r
		}
	}
	if _, err := cache.Put(creds.Certificate.Certificate, now); err != nil {
		return nil, fmt.Errorf("cache own mesh certificate: %w", err)
	}
	return &Catalog{mesh: mesh, org: org, asset: asset, limit: limit, creds: creds,
		cache: cache, authorize: authorize, persist: persist,
		entries: make(map[Key]storedRecord), history: history}, nil
}

func cloneWire(w SignedRecord) SignedRecord {
	w.Body = append(json.RawMessage(nil), w.Body...)
	w.Signature = append([]byte(nil), w.Signature...)
	return w
}

func (c *Catalog) prune(now time.Time) {
	for key, entry := range c.entries {
		if !time.UnixMilli(entry.record.Expires).After(now) {
			delete(c.entries, key)
		}
	}
	for key, receipt := range c.history {
		if key.Asset != c.asset && !receipt.Until.After(now) {
			delete(c.history, key)
		}
	}
}

func sortedReceipts(m map[Key]Receipt) []Receipt {
	out := make([]Receipt, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Key, out[j].Key) })
	return out
}

func lessKey(a, b Key) bool {
	if a.Mesh != b.Mesh {
		return a.Mesh < b.Mesh
	}
	if a.Org != b.Org {
		return a.Org < b.Org
	}
	if a.Asset != b.Asset {
		return a.Asset < b.Asset
	}
	if a.AppID != b.AppID {
		return a.AppID < b.AppID
	}
	return a.ServiceID < b.ServiceID
}

// Accept verifies the origin signature against current trust. A duplicate
// cannot renew the signed absolute expiry. A relaying neighbour cannot change
// the service owner, endpoint, generation or lease without invalidating it.
func (c *Catalog) Accept(w SignedRecord, now time.Time) (bool, error) {
	r, hash, err := verify(w, c.cache, now)
	if err != nil {
		return false, err
	}
	if r.Key.Mesh != c.mesh || r.Key.Org != c.org {
		return false, errors.New("mesh service belongs to another scope")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	if old, found := c.history[r.Key]; found {
		if r.Generation < old.Generation {
			return false, ErrStale
		}
		if r.Generation == old.Generation {
			if hash != old.Hash {
				return false, errors.New("conflicting mesh service generation")
			}
			// Receipts survive a restart, but signed records do not. Rebuild
			// the live record from an identical, freshly verified peer copy.
			// A receipt for a later generation still rejects stale publishes.
			if _, live := c.entries[r.Key]; live {
				return false, nil
			}
			if len(c.entries) >= c.limit {
				return false, errors.New("mesh catalog capacity reached")
			}
			c.entries[r.Key] = storedRecord{r, cloneWire(w), hash}
			return true, nil
		}
	} else if len(c.history) >= c.limit {
		return false, errors.New("mesh catalog receipt capacity reached")
	}
	if _, found := c.entries[r.Key]; !found && len(c.entries) >= c.limit {
		return false, errors.New("mesh catalog capacity reached")
	}
	next := make(map[Key]Receipt, len(c.history)+1)
	for key, old := range c.history {
		next[key] = old
	}
	leaseLimit := MaxLease
	if IsGatewayOffer(r) {
		leaseLimit = GatewayOfferLease
	}
	until := now.Add(leaseLimit + 10*time.Second)
	if r.Key.Asset == c.asset {
		// A local generation must never be reused, even after a restart or
		// a longer disconnect than the remote replay window.
		until = time.Time{}
	}
	next[r.Key] = Receipt{r.Key, r.Generation, hash, until}
	if err := c.persist(sortedReceipts(next)); err != nil {
		return false, fmt.Errorf("persist mesh service generation: %w", err)
	}
	c.history = next
	c.entries[r.Key] = storedRecord{r, cloneWire(w), hash}
	return true, nil
}

func (c *Catalog) nextGeneration(key Key, now time.Time) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	r := c.history[key]
	if r.Generation == ^uint64(0) {
		return 0, errors.New("mesh service generation exhausted")
	}
	return r.Generation + 1, nil
}

// Publish signs an authorized local app service and admits it transactionally.
// The app layer supplies its authoritative app ID; TXT is never an authority.
func (c *Catalog) Publish(spec PublishSpec, now time.Time) (SignedRecord, error) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	if spec.AppID == GatewayAppID || spec.ServiceID == GatewayServiceID {
		return SignedRecord{}, errors.New("reserved mesh gateway identity cannot be published by an app")
	}
	if err := c.authorize(spec.AppID, spec.Type, spec.HostPort); err != nil {
		return SignedRecord{}, err
	}
	key := Key{c.mesh, c.org, c.asset, spec.AppID, spec.ServiceID}
	gen, err := c.nextGeneration(key, now)
	if err != nil {
		return SignedRecord{}, err
	}
	r, err := spec.record(key, gen, now)
	if err != nil {
		return SignedRecord{}, err
	}
	w, err := Sign(r, c.creds.Certificate.Certificate, c.creds.Signer, now)
	if err != nil {
		return SignedRecord{}, err
	}
	if _, err = c.Accept(w, now); err != nil {
		return SignedRecord{}, err
	}
	return w, nil
}

// Remove signs a tombstone even if a local app has already stopped. The caller
// must derive appID from the stopped container's trusted lifecycle identity.
func (c *Catalog) Remove(appID, serviceID string, now time.Time) (SignedRecord, error) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	if appID == GatewayAppID || serviceID == GatewayServiceID {
		return SignedRecord{}, errors.New("reserved mesh gateway identity cannot be withdrawn by an app")
	}
	key := Key{c.mesh, c.org, c.asset, appID, serviceID}
	c.mu.Lock()
	c.prune(now)
	_, found := c.history[key]
	c.mu.Unlock()
	if !found {
		return SignedRecord{}, errors.New("unknown local mesh service")
	}
	gen, err := c.nextGeneration(key, now)
	if err != nil {
		return SignedRecord{}, err
	}
	r := Record{Version: 1, Key: key, Generation: gen, Issued: now.UnixMilli(),
		Expires: now.Add(MaxLease).UnixMilli(), Withdraw: true}
	w, err := Sign(r, c.creds.Certificate.Certificate, c.creds.Signer, now)
	if err != nil {
		return SignedRecord{}, err
	}
	if _, err = c.Accept(w, now); err != nil {
		return SignedRecord{}, err
	}
	return w, nil
}

// PublishGatewayOffer signs the agent-owned gateway capability. It has no
// endpoint, so it cannot authorize an app port or appear in DNS-SD projection.
func (c *Catalog) PublishGatewayOffer(now time.Time) (SignedRecord, error) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	key := Key{c.mesh, c.org, c.asset, GatewayAppID, GatewayServiceID}
	gen, err := c.nextGeneration(key, now)
	if err != nil {
		return SignedRecord{}, err
	}
	r := Record{Version: 1, Key: key, Generation: gen,
		Issued: now.UnixMilli(), Expires: now.Add(GatewayOfferLease).UnixMilli()}
	w, err := Sign(r, c.creds.Certificate.Certificate, c.creds.Signer, now)
	if err != nil {
		return SignedRecord{}, err
	}
	if _, err := c.Accept(w, now); err != nil {
		return SignedRecord{}, err
	}
	return w, nil
}

// WithdrawGatewayOffer signs a tombstone. An empty record means there was no
// local gateway history to withdraw or the current entry is already withdrawn.
func (c *Catalog) WithdrawGatewayOffer(now time.Time) (SignedRecord, error) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	key := Key{c.mesh, c.org, c.asset, GatewayAppID, GatewayServiceID}
	c.mu.Lock()
	c.prune(now)
	_, found := c.history[key]
	entry, live := c.entries[key]
	c.mu.Unlock()
	if !found || (live && entry.record.Withdraw) {
		return SignedRecord{}, nil
	}
	gen, err := c.nextGeneration(key, now)
	if err != nil {
		return SignedRecord{}, err
	}
	r := Record{Version: 1, Key: key, Generation: gen,
		Issued: now.UnixMilli(), Expires: now.Add(GatewayOfferLease).UnixMilli(), Withdraw: true}
	w, err := Sign(r, c.creds.Certificate.Certificate, c.creds.Signer, now)
	if err != nil {
		return SignedRecord{}, err
	}
	if _, err := c.Accept(w, now); err != nil {
		return SignedRecord{}, err
	}
	return w, nil
}

// GatewayOffers returns active, signed, unexpired gateway capabilities whose
// origin certificate remains trusted by this catalog. Route availability is
// a separate decision for the mesh sharing policy.
func (c *Catalog) GatewayOffers(now time.Time) []Record {
	all := c.Snapshot(now)
	out := make([]Record, 0)
	for _, r := range all {
		if IsGatewayOffer(r) {
			out = append(out, r)
		}
	}
	return out
}

func (c *Catalog) Snapshot(now time.Time) []Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	out := make([]Record, 0, len(c.entries))
	for _, entry := range c.entries {
		if entry.record.Withdraw {
			continue
		}
		if _, _, ok := c.cache.Get(entry.wire.Fingerprint, now); !ok {
			continue
		}
		r := entry.record
		r.TXT = append([]string(nil), r.TXT...)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Key, out[j].Key) })
	return out
}

// Records includes still-live tombstones for reconnect and anti-entropy.
func (c *Catalog) Records(now time.Time) []SignedRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	keys := make([]Key, 0, len(c.entries))
	for key, entry := range c.entries {
		if _, _, ok := c.cache.Get(entry.wire.Fingerprint, now); ok {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return lessKey(keys[i], keys[j]) })
	out := make([]SignedRecord, 0, len(keys))
	for _, key := range keys {
		out = append(out, cloneWire(c.entries[key].wire))
	}
	return out
}

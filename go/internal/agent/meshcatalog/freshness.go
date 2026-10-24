package meshcatalog

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const freshDomain = "wendy-mesh-service/inventory/1\x00"
const maxFreshBytes = 4 << 20
const freshChunkBytes = 8 << 10
const freshLifetime = 30 * time.Second

type FreshRequest struct {
	Origin    string `json:"origin"`
	Requester string `json:"requester"`
	Nonce     string `json:"nonce"`
	Hops      uint8  `json:"hops"`
}

func (q FreshRequest) validate() error {
	a, e := meshid.DeviceFromPrincipal(q.Origin)
	if e != nil {
		return e
	}
	b, e := meshid.DeviceFromPrincipal(q.Requester)
	if e != nil {
		return e
	}
	if a.Tenant() != b.Tenant() || q.Hops == 0 || q.Hops > 32 || !validNonce(q.Nonce) {
		return errors.New("invalid freshness request")
	}
	return nil
}
func validNonce(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

type FreshChunk struct {
	Nonce string `json:"nonce"`
	Index int    `json:"index"`
	Total int    `json:"total"`
	Data  []byte `json:"data"`
}

func (c FreshChunk) validate() error {
	if !validNonce(c.Nonce) || c.Index < 0 || c.Total <= 0 || c.Total > (maxFreshBytes+freshChunkBytes-1)/freshChunkBytes || c.Index >= c.Total || len(c.Data) == 0 || len(c.Data) > freshChunkBytes {
		return errors.New("invalid freshness chunk")
	}
	return nil
}

type freshReceipt struct {
	Key        Key    `json:"key"`
	Generation uint64 `json:"generation"`
	Hash       string `json:"hash"`
	Live       bool   `json:"live"`
}
type originInventory struct {
	Mesh      string         `json:"mesh"`
	Origin    string         `json:"origin"`
	Requester string         `json:"requester"`
	Nonce     string         `json:"nonce"`
	Issued    int64          `json:"issued"`
	Expires   int64          `json:"expires"`
	Receipts  []freshReceipt `json:"receipts"`
}
type signedInventory struct {
	Body      json.RawMessage             `json:"body"`
	Signature []byte                      `json:"signature"`
	Chain     *localmesh.CertificateChain `json:"chain"`
}

func inventorySignInput(key crypto.PublicKey, body []byte) ([]byte, crypto.SignerOpts) {
	b := append([]byte(freshDomain), body...)
	// Ed25519 and ML-DSA sign the message itself; Go's signers refuse a digest.
	switch key.(type) {
	case ed25519.PublicKey, *mldsa.PublicKey:
		return b, crypto.Hash(0)
	}
	h := sha256.Sum256(b)
	return h[:], crypto.SHA256
}
func (c *Catalog) signInventory(q FreshRequest, now time.Time) ([]byte, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	if q.Origin != c.principal {
		return nil, errors.New("wrong inventory origin")
	}
	// Snapshot all own durable high-waters, including records absent after restart.
	// A live-only list could resurrect an older unexpired service body.
	c.mutation.Lock()
	defer c.mutation.Unlock()
	c.mu.Lock()
	c.prune(now)
	receipts := sortedReceipts(c.history)
	body := originInventory{Mesh: c.mesh, Origin: c.principal, Requester: q.Requester, Nonce: q.Nonce, Issued: now.UnixMilli(), Expires: now.Add(freshLifetime).UnixMilli(), Receipts: []freshReceipt{}}
	for _, receipt := range receipts {
		if receipt.Key.Device == c.principal {
			e, live := c.entries[receipt.Key]
			live = live && !e.record.Withdraw
			body.Receipts = append(body.Receipts, freshReceipt{receipt.Key, receipt.Generation, receipt.Hash, live})
		}
	}
	c.mu.Unlock()
	_, id, ok := c.cache.Get(c.principal, now)
	if !ok {
		return nil, ErrIdentityNeeded
	}
	body.Expires = min(body.Expires, id.NotAfter.UnixMilli())
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	input, opts := inventorySignInput(c.creds.Signer.Public(), raw)
	sig, err := c.creds.Signer.Sign(rand.Reader, input, opts)
	if err != nil {
		return nil, err
	}
	description := localmesh.DescribeCertificates(c.principal, c.creds.Certificate.Certificate)
	if description == nil {
		return nil, errors.New("invalid origin inventory certificate chain")
	}
	data, err := json.Marshal(signedInventory{Body: raw, Signature: sig, Chain: description.Chain})
	if len(data) > maxFreshBytes {
		return nil, errors.New("origin inventory exceeds bound")
	}
	return data, err
}
func (c *Catalog) acceptInventory(data []byte, q FreshRequest, now time.Time) error {
	if len(data) == 0 || len(data) > maxFreshBytes {
		return errors.New("invalid inventory size")
	}
	var signed signedInventory
	var body originInventory
	if err := json.Unmarshal(data, &signed); err != nil {
		return err
	}
	if len(signed.Signature) == 0 || len(signed.Signature) > 8192 || signed.Chain == nil { // ML-DSA-65 signs in 3309 bytes
		return errors.New("invalid inventory envelope")
	}
	if err := json.Unmarshal(signed.Body, &body); err != nil {
		return err
	}
	if body.Mesh != c.mesh || body.Origin != q.Origin || body.Requester != c.principal || body.Requester != q.Requester || body.Nonce != q.Nonce || len(body.Receipts) > c.limit || signed.Chain.Principal != body.Origin {
		return errors.New("inventory binding mismatch")
	}
	origin, err := meshid.DeviceFromPrincipal(body.Origin)
	if err != nil || origin.Tenant() != c.device.Tenant() {
		return errors.New("foreign inventory origin")
	}
	expires, issued := time.UnixMilli(body.Expires), time.UnixMilli(body.Issued)
	if !expires.After(now) || issued.After(now.Add(5*time.Second)) || !expires.After(issued) || expires.Sub(issued) > freshLifetime {
		return ErrExpired
	}
	principal, err := c.cache.ResolveCertificateChain(*signed.Chain, now)
	if err != nil {
		if errors.Is(err, localmesh.ErrCertificateNeeded) {
			return ErrIdentityNeeded
		}
		return err
	}
	if principal != body.Origin {
		return errors.New("inventory signer mismatch")
	}
	chain, identity, ok := c.cache.Get(principal, now)
	if !ok || expires.After(identity.NotAfter) {
		return errors.New("inventory outlives identity")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return err
	}
	input, _ := inventorySignInput(leaf.PublicKey, signed.Body)
	valid := false
	switch key := leaf.PublicKey.(type) {
	case *mldsa.PublicKey:
		valid = mldsa.Verify(key, input, signed.Signature, nil) == nil
	case ed25519.PublicKey:
		valid = ed25519.Verify(key, input, signed.Signature)
	case *ecdsa.PublicKey:
		valid = ecdsa.VerifyASN1(key, input, signed.Signature)
	case *rsa.PublicKey:
		valid = rsa.VerifyPKCS1v15(key, crypto.SHA256, input, signed.Signature) == nil
	}
	if !valid {
		return errors.New("invalid inventory signature")
	}
	seen := make(map[Key]bool, len(body.Receipts))
	for i, v := range body.Receipts {
		hash, e := hex.DecodeString(v.Hash)
		if v.Key.Mesh != c.mesh || v.Key.Device != body.Origin || appconfig.ValidateAppID(v.Key.AppID) != nil || !labelPattern.MatchString(v.Key.ServiceID) || v.Generation == 0 || e != nil || len(hash) != 32 || hex.EncodeToString(hash) != v.Hash || seen[v.Key] || (i > 0 && !lessKey(body.Receipts[i-1].Key, v.Key)) {
			return errors.New("invalid inventory receipt")
		}
		seen[v.Key] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	next := make(map[Key]Receipt, len(c.history)+len(seen))
	for k, v := range c.history {
		next[k] = v
	}
	for _, v := range body.Receipts {
		old := next[v.Key]
		if old.Generation > v.Generation {
			continue
		}
		if old.Generation == v.Generation && old.Hash != v.Hash {
			return errors.New("conflicting origin inventory generation")
		}
		// Gateway offers have longer leases than ordinary services.
		next[v.Key] = Receipt{v.Key, v.Generation, v.Hash, now.Add(GatewayOfferLease + 10*time.Second)}
	}
	if len(next) > c.limit {
		return errors.New("inventory receipt capacity reached")
	}
	if err := c.persist(sortedReceipts(next)); err != nil {
		return err
	}
	c.history = next
	if c.unavailable == nil {
		c.unavailable = make(map[Key]uint64)
	}
	for _, v := range body.Receipts {
		if c.history[v.Key].Generation > v.Generation {
			continue
		}
		if !v.Live {
			c.unavailable[v.Key] = v.Generation
		} else if c.unavailable[v.Key] < v.Generation {
			delete(c.unavailable, v.Key)
		}
		if entry, ok := c.entries[v.Key]; ok && (entry.record.Generation < v.Generation || (!v.Live && !entry.record.Withdraw)) {
			delete(c.entries, v.Key)
		}
	}
	return nil
}

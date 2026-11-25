package agentvmfixture

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
)

type EnrollmentRequest struct {
	DirectoryURL string `json:"directory_url"`
	DeviceID     string `json:"device_id"`
	EABKeyID     string `json:"eab_key_id"`
	EABHMACKey   string `json:"eab_hmac_key"`
	CloudHost    string `json:"cloud_host"`
}
type ListenerReceipt struct {
	Kind                  string    `json:"kind"`
	Address               string    `json:"address"`
	EnrollmentRequestPath string    `json:"enrollment_request_path"`
	Expires               time.Time `json:"expires"`
}
type IssuedReceipt struct {
	Kind               string `json:"kind"`
	Principal          string `json:"principal"`
	LeafDER            []byte `json:"leaf_der"`
	LeafSHA256         string `json:"leaf_sha256"`
	DeviceSPKISHA256   string `json:"device_spki_sha256"`
	CSRSHA256          string `json:"csr_sha256"`
	CSRVerified        bool   `json:"csr_verified"`
	EABVerified        bool   `json:"eab_verified"`
	AccountJWSVerified bool   `json:"account_jws_verified"`
}

// Serve binds only a numeric loopback address and issues at most one generated
// test-device certificate. It writes no secret to stdout or HTTP error bodies.
// The normal agent enrolls using enrollment-request.json through its V2 RPC.
func Serve(ctx context.Context, directory, address, handoff string, duration time.Duration, ready func(ListenerReceipt)) error {
	if duration <= 0 || duration > 10*time.Minute {
		return errors.New("fixture duration must be within ten minutes")
	}
	if !filepath.IsAbs(handoff) || filepath.Clean(handoff) != handoff || filepath.Base(handoff) != "acme-enrollment.json" {
		return errors.New("absolute acme-enrollment.json handoff path required")
	}
	if err := hostpath.EnsureDirectory(filepath.Dir(handoff), 0700, true); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("numeric loopback listener required")
	}
	a, err := load(directory)
	if err != nil {
		return err
	}
	// A prior successful issuance is never silently repeated on restart.
	if _, err = os.Lstat(receiptPath(directory, "issued.json")); !os.IsNotExist(err) {
		return errors.New("fixture already issued or issuance state unavailable")
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer ln.Close()
	base := "http://" + ln.Addr().String()
	s := &acmeServer{authority: a, base: base, nonces: map[string]bool{}}
	if err = writeJSON(receiptPath(directory, "enrollment-request.json"), EnrollmentRequest{DirectoryURL: base + "/" + a.public.Tenant + "/acme/directory", DeviceID: a.public.DeviceID, EABKeyID: a.secret.EABKeyID, EABHMACKey: hex.EncodeToString(a.secret.EABKey), CloudHost: ln.Addr().String()}); err != nil {
		return err
	}
	// The actual agent consumes this normal first-boot handoff. This fixture
	// neither calls provisioning internals nor writes provisioned device state.
	if err = writeJSON(handoff, map[string]string{"directoryURL": base + "/" + a.public.Tenant + "/acme/directory", "deviceID": a.public.DeviceID, "eabKeyID": a.secret.EABKeyID, "eabHMACKey": hex.EncodeToString(a.secret.EABKey), "cloudHost": ln.Addr().String()}); err != nil {
		return err
	}
	r := ListenerReceipt{Kind: Kind, Address: ln.Addr().String(), EnrollmentRequestPath: receiptPath(directory, "enrollment-request.json"), Expires: time.Now().Add(duration).UTC()}
	if err = writeJSON(receiptPath(directory, "listener.json"), r); err != nil {
		return err
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	defer close(done)
	if ready != nil {
		ready(r)
	}
	err = server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type acmeServer struct {
	mu         sync.Mutex
	authority  *authority
	base       string
	nonces     map[string]bool
	requests   int
	account    *ecdsa.PublicKey
	accountJWK []byte
	ordered    bool
	csr        []byte
	chain      []byte
}

type envelope struct {
	Protected string `json:"protected"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}
type header struct {
	Alg   string `json:"alg"`
	Nonce string `json:"nonce,omitempty"`
	URL   string `json:"url"`
	KID   string `json:"kid,omitempty"`
	JWK   *jwk   `json:"jwk,omitempty"`
}

func decode64(s string) ([]byte, error) {
	b, e := base64.RawURLEncoding.DecodeString(s)
	if e != nil || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, errors.New("noncanonical base64url")
	}
	return b, nil
}
func (s *acmeServer) nonce(w http.ResponseWriter) error {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	n := base64.RawURLEncoding.EncodeToString(b)
	s.nonces[n] = true
	w.Header().Set("Replay-Nonce", n)
	return nil
}
func response(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter) {
	response(w, http.StatusBadRequest, map[string]string{"type": "urn:ietf:params:acme:error:unauthorized", "detail": "disposable fixture request refused"})
}
func (s *acmeServer) order() map[string]any {
	status := "ready"
	if len(s.chain) > 0 {
		status = "valid"
	}
	r := map[string]any{"status": status, "identifiers": []map[string]string{{"type": "permanent-identifier", "value": s.authority.public.DeviceID}}, "finalize": s.base + "/acme/finalize"}
	if len(s.chain) > 0 {
		r["certificate"] = s.base + "/" + s.authority.public.Tenant + "/acme/cert/1"
	}
	return r
}
func (s *acmeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	if s.requests > 256 || !time.Now().Before(s.authority.public.NotAfter) || r.Host != strings.TrimPrefix(s.base, "http://") || r.URL.RawQuery != "" || r.URL.EscapedPath() != r.URL.Path {
		problem(w)
		return
	}
	if err := s.nonce(w); err != nil {
		problem(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/"+s.authority.public.Tenant+"/acme/directory" && r.Method == http.MethodGet {
		response(w, 200, map[string]any{"newNonce": s.base + "/acme/new-nonce", "newAccount": s.base + "/acme/new-account", "newOrder": s.base + "/acme/new-order", "meta": map[string]bool{"externalAccountRequired": true}})
		return
	}
	if r.URL.Path == "/acme/new-nonce" && (r.Method == http.MethodHead || r.Method == http.MethodGet) {
		w.WriteHeader(200)
		return
	}
	certPath := "/" + s.authority.public.Tenant + "/acme/cert/1"
	if r.URL.Path == certPath && r.Method == http.MethodGet && len(s.chain) > 0 {
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = w.Write(s.chain)
		return
	}
	if r.Method != http.MethodPost {
		problem(w)
		return
	}
	newAccount := r.URL.Path == "/acme/new-account"
	payload, account, accountJWK, err := s.authenticate(r, newAccount)
	if err != nil {
		problem(w)
		return
	}
	switch r.URL.Path {
	case "/acme/new-account":
		var req struct {
			TermsOfServiceAgreed   bool     `json:"termsOfServiceAgreed"`
			ExternalAccountBinding envelope `json:"externalAccountBinding"`
			Contact                []string `json:"contact,omitempty"`
			OnlyReturnExisting     bool     `json:"onlyReturnExisting,omitempty"`
		}
		if jws.DecodeStrict(payload, &req) != nil || req.OnlyReturnExisting || len(req.Contact) != 0 || s.verifyEAB(req.ExternalAccountBinding, accountJWK) != nil {
			problem(w)
			return
		}
		if s.account != nil && !s.account.Equal(account) {
			problem(w)
			return
		}
		status := http.StatusCreated
		if s.account != nil {
			status = http.StatusOK
		}
		s.account, s.accountJWK = account, accountJWK
		w.Header().Set("Location", s.base+"/acme/account/1")
		response(w, status, map[string]string{"status": "valid", "orders": s.base + "/acme/orders"})
	case "/acme/new-order":
		var req struct {
			Identifiers []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"identifiers"`
			NotBefore string `json:"notBefore,omitempty"`
			NotAfter  string `json:"notAfter,omitempty"`
		}
		if jws.DecodeStrict(payload, &req) != nil || len(req.Identifiers) != 1 || req.Identifiers[0].Type != "permanent-identifier" || req.Identifiers[0].Value != s.authority.public.DeviceID || req.NotBefore != "" || req.NotAfter != "" || len(s.chain) > 0 {
			problem(w)
			return
		}
		s.ordered = true
		w.Header().Set("Location", s.base+"/acme/order/1")
		response(w, http.StatusCreated, s.order())
	case "/acme/finalize":
		var req struct {
			CSR string `json:"csr"`
		}
		if !s.ordered || jws.DecodeStrict(payload, &req) != nil {
			problem(w)
			return
		}
		csr, err := decode64(req.CSR)
		if err != nil {
			problem(w)
			return
		}
		if len(s.chain) > 0 {
			if !bytes.Equal(s.csr, csr) {
				problem(w)
				return
			}
		} else if s.issue(csr) != nil {
			problem(w)
			return
		}
		w.Header().Set("Location", s.base+"/acme/order/1")
		response(w, 200, s.order())
	case "/acme/order/1":
		if len(payload) != 0 || !s.ordered {
			problem(w)
			return
		}
		response(w, 200, s.order())
	case certPath:
		if len(payload) != 0 || len(s.chain) == 0 {
			problem(w)
			return
		}
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = w.Write(s.chain)
	default:
		problem(w)
	}
}

func parseJWK(j *jwk) (*ecdsa.PublicKey, []byte, error) {
	if j == nil || j.Kty != "EC" || j.Crv != "P-256" {
		return nil, nil, safeError("account")
	}
	x, e := decode64(j.X)
	if e != nil || len(x) != 32 {
		return nil, nil, safeError("account")
	}
	y, e := decode64(j.Y)
	if e != nil || len(y) != 32 {
		return nil, nil, safeError("account")
	}
	k := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !k.Curve.IsOnCurve(k.X, k.Y) {
		return nil, nil, safeError("account")
	}
	canonical, _ := json.Marshal(j)
	return k, canonical, nil
}
func (s *acmeServer) authenticate(r *http.Request, newAccount bool) ([]byte, *ecdsa.PublicKey, []byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 64<<10))
	if err != nil {
		return nil, nil, nil, safeError("body")
	}
	var env envelope
	if jws.DecodeStrict(b, &env) != nil {
		return nil, nil, nil, safeError("body")
	}
	protected, err := decode64(env.Protected)
	var h header
	if err != nil || jws.DecodeStrict(protected, &h) != nil || h.Alg != "ES256" || h.URL != s.base+r.URL.Path || h.Nonce == "" || !s.nonces[h.Nonce] {
		return nil, nil, nil, safeError("JWS header")
	}
	delete(s.nonces, h.Nonce)
	var key *ecdsa.PublicKey
	var encoded []byte
	if newAccount {
		if h.KID != "" {
			return nil, nil, nil, safeError("account")
		}
		key, encoded, err = parseJWK(h.JWK)
	} else {
		if h.JWK != nil || h.KID != s.base+"/acme/account/1" || s.account == nil {
			return nil, nil, nil, safeError("account")
		}
		key, encoded = s.account, s.accountJWK
	}
	if err != nil {
		return nil, nil, nil, err
	}
	sig, err := decode64(env.Signature)
	if err != nil || len(sig) != 64 {
		return nil, nil, nil, safeError("JWS signature")
	}
	hash := sha256.Sum256([]byte(env.Protected + "." + env.Payload))
	if !ecdsa.Verify(key, hash[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, nil, nil, safeError("JWS signature")
	}
	payload, err := decode64(env.Payload)
	if err != nil {
		return nil, nil, nil, err
	}
	return payload, key, encoded, nil
}
func (s *acmeServer) verifyEAB(env envelope, accountJWK []byte) error {
	protected, err := decode64(env.Protected)
	var h header
	if err != nil || jws.DecodeStrict(protected, &h) != nil || h.Alg != "HS256" || h.KID != s.authority.secret.EABKeyID || h.URL != s.base+"/acme/new-account" || h.Nonce != "" || h.JWK != nil {
		return safeError("EAB header")
	}
	payload, err := decode64(env.Payload)
	var j jwk
	if err != nil || jws.DecodeStrict(payload, &j) != nil {
		return safeError("EAB body")
	}
	_, canonical, err := parseJWK(&j)
	if err != nil || !bytes.Equal(canonical, accountJWK) {
		return safeError("EAB account")
	}
	sig, err := decode64(env.Signature)
	if err != nil {
		return safeError("EAB signature")
	}
	mac := hmac.New(sha256.New, s.authority.secret.EABKey)
	_, _ = mac.Write([]byte(env.Protected + "." + env.Payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return safeError("EAB signature")
	}
	return nil
}
func (s *acmeServer) issue(raw []byte) error {
	csr, err := x509.ParseCertificateRequest(raw)
	if err != nil || csr.CheckSignature() != nil {
		return safeError("CSR signature")
	}
	key, ok := csr.PublicKey.(*mldsa.PublicKey)
	if !ok || key.Parameters() != mldsa.MLDSA65() {
		return safeError("CSR key")
	}
	// Normal agent enrollment identifies its ordered device in the CSR CN.
	// Paired issuers require this; the single-node fixture remains unchanged.
	if s.authority.public.PairID != "" && (csr.Subject.CommonName != s.authority.public.DeviceID || len(csr.URIs) != 0 || len(csr.DNSNames) != 0 || len(csr.EmailAddresses) != 0 || len(csr.IPAddresses) != 0) {
		return safeError("CSR target")
	}
	serial := uint64(3)
	if s.authority.public.PairID != "" {
		serial = s.authority.public.LeafSerial
	}
	principal, _ := url.Parse(s.authority.public.Principal)
	template := &x509.Certificate{SerialNumber: new(big.Int).SetUint64(serial), Subject: pkix.Name{CommonName: "UNTRUSTED DISPOSABLE VM DEVICE"}, NotBefore: s.authority.public.NotBefore, NotAfter: s.authority.public.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{principal}}
	der, err := x509.CreateCertificate(rand.Reader, template, s.authority.issuer, key, s.authority.key)
	if err != nil {
		return safeError("certificate")
	}
	spki, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return safeError("key encoding")
	}
	receipt := IssuedReceipt{Kind: Kind, Principal: s.authority.public.Principal, LeafDER: der, LeafSHA256: digest(der), DeviceSPKISHA256: digest(spki), CSRSHA256: digest(raw), CSRVerified: true, EABVerified: true, AccountJWSVerified: true}
	// Durably publish the public receipt before exposing this certificate. A
	// failed or uncertain write blocks another issuance, including after restart.
	if writeJSON(receiptPath(s.authority.directory, "issued.json"), receipt) != nil {
		return safeError("issuance persistence")
	}
	for _, b := range [][]byte{der, s.authority.public.IssuerDER, s.authority.public.RootDER} {
		s.chain = append(s.chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b})...)
	}
	s.csr = bytes.Clone(raw)
	return nil
}

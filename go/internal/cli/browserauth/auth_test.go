package browserauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(b))}
}
func setup(t *testing.T, alter func(map[string]any)) (*Session, *int) {
	t.Helper()
	authority, e := mldsa.GenerateKey(mldsa.MLDSA65())
	if e != nil {
		t.Fatal(e)
	}
	issuer := AuthBase + "/realms/system"
	calls := 0
	nonceSent := false
	lastRefresh := ""
	var state, challenge string
	s := &Session{}
	s.Client = doerFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case issuer + "/.well-known/openid-configuration":
			return reply(200, metadata{Issuer: issuer, Authorize: issuer + "/authorize", Token: issuer + "/oauth2/token", JWKS: issuer + "/.well-known/jwks.json"}), nil
		case issuer + "/.well-known/jwks.json":
			return reply(200, map[string]any{"keys": []any{map[string]string{"kid": "test", "alg": "ML-DSA-65", "kty": "AKP", "pub": base64.RawURLEncoding.EncodeToString(authority.Public().(*mldsa.PublicKey).Bytes())}}}), nil
		case issuer + "/userinfo":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "DPoP ") || r.Header.Get("DPoP") == "" {
				t.Fatal("missing UserInfo proof")
			}
			return reply(200, map[string]string{"sub": "operator-test", "email": "operator@example.test"}), nil
		case issuer + "/oauth2/token":
			calls++
			raw, _ := io.ReadAll(r.Body)
			f, _ := url.ParseQuery(string(raw))
			if f.Get("client_id") != ClientID {
				t.Fatal("wrong OAuth client")
			}
			pieces := strings.Split(r.Header.Get("DPoP"), ".")
			if len(pieces) != 3 {
				t.Fatal("missing DPoP proof")
			}
			decode := base64.RawURLEncoding.DecodeString
			hdr, _ := decode(pieces[0])
			payload, _ := decode(pieces[1])
			signature, _ := decode(pieces[2])
			var h struct {
				JWK map[string]string `json:"jwk"`
			}
			_ = json.Unmarshal(hdr, &h)
			pub, _ := decode(h.JWK["pub"])
			key, e := mldsa.NewPublicKey(mldsa.MLDSA65(), pub)
			if e != nil {
				t.Fatal(e)
			}
			if e = mldsa.Verify(key, []byte(pieces[0]+"."+pieces[1]), signature, nil); e != nil {
				t.Fatal("invalid DPoP signature", e)
			}
			var proof map[string]any
			_ = json.Unmarshal(payload, &proof)
			if proof["htu"] != r.URL.String() || proof["htm"] != "POST" {
				t.Fatal("wrong proof target")
			}
			if !nonceSent {
				nonceSent = true
				response := reply(400, map[string]string{"error": "use_dpop_nonce"})
				response.Header.Set("DPoP-Nonce", "test-nonce")
				return response, nil
			}
			if f.Get("grant_type") == "authorization_code" {
				if proof["nonce"] != "test-nonce" {
					t.Fatal("nonce was not retried")
				}
				hash := sha256.Sum256([]byte(f.Get("code_verifier")))
				if base64.RawURLEncoding.EncodeToString(hash[:]) != challenge || f.Get("code") != "valid-code" || state == "" {
					t.Fatal("PKCE or code mismatch")
				}
			} else if f.Get("refresh_token") != lastRefresh {
				t.Fatal("wrong refresh token")
			}
			canonical, _ := json.Marshal(h.JWK)
			thumb := sha256.Sum256(canonical)
			claims := map[string]any{"iss": issuer, "aud": f.Get("resource"), "sub": "operator-test", "tenant_uuid": "12345678-1234-1234-1234-123456789abc", "exp": time.Now().Add(time.Hour).Unix(), "cnf": map[string]string{"jkt": base64.RawURLEncoding.EncodeToString(thumb[:])}}
			if alter != nil {
				alter(claims)
			}
			head, _ := json.Marshal(map[string]string{"kid": "test", "alg": "ML-DSA-65"})
			body, _ := json.Marshal(claims)
			input := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
			sig, e := authority.Sign(rand.Reader, []byte(input), crypto.Hash(0))
			if e != nil {
				t.Fatal(e)
			}
			lastRefresh = fmt.Sprintf("refresh-%d", calls)
			return reply(200, token{Access: input + "." + base64.RawURLEncoding.EncodeToString(sig), Refresh: lastRefresh, Type: "DPoP", Expires: 3600}), nil
		case IdentityEndpoint:
			if !strings.HasPrefix(r.Header.Get("Authorization"), "DPoP ") || r.Header.Get("Content-Type") != "application/pkcs10" {
				t.Fatal("missing PKI binding")
			}
			b, _ := io.ReadAll(r.Body)
			block, _ := pem.Decode(b)
			if block == nil {
				t.Fatal("missing CSR")
			}
			csr, e := x509.ParseCertificateRequest(block.Bytes)
			if e != nil {
				t.Fatal(e)
			}
			if e = csr.CheckSignature(); e != nil {
				t.Fatal(e)
			}
			uri, _ := url.Parse("spiffe://wendy.sh/tenant/12345678-1234-1234-1234-123456789abc/operator/operator-test")
			leaf := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "operator-test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature}
			der, e := x509.CreateCertificate(rand.Reader, leaf, leaf, csr.PublicKey, authority)
			if e != nil {
				t.Fatal(e)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))}, nil
		}
		t.Fatalf("unexpected outbound endpoint %s", r.URL)
		return nil, nil
	})
	auth, e := s.Begin(context.Background(), "", "http://localhost:5173/auth/callback")
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse(auth)
	state = u.Query().Get("state")
	challenge = u.Query().Get("code_challenge")
	if strings.Contains(auth, "+") || u.Query().Get("resource") != IdentityResource || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("invalid authorize request")
	}
	return s, &calls
}
func TestBrowserLoginBoundCertificateAndRefresh(t *testing.T) {
	s, calls := setup(t, nil)
	state := s.state
	profile, e := s.Complete(context.Background(), "valid-code", state, s.meta.Issuer)
	if e != nil {
		t.Fatal(e)
	}
	if profile.Tenant != "12345678-1234-1234-1234-123456789abc" || profile.Name != "operator@example.test" || s.certificate.PemPrivateKey == "" || *calls != 3 {
		t.Fatal("incomplete identity or refresh exchange")
	}
	if _, e = s.Complete(context.Background(), "valid-code", state, s.meta.Issuer); e == nil {
		t.Fatal("accepted replay")
	}
}
func TestBrowserLoginRejectsCallbackBeforeExchange(t *testing.T) {
	for _, scenario := range []string{"state", "expired", "issuer", "missing-code"} {
		t.Run(scenario, func(t *testing.T) {
			s, calls := setup(t, nil)
			state, issuer, code := s.state, s.meta.Issuer, "valid-code"
			switch scenario {
			case "state":
				state = "wrong"
			case "expired":
				s.started = time.Now().Add(-11 * time.Minute)
			case "issuer":
				issuer = "https://attacker.test/realms/system"
			case "missing-code":
				code = ""
			}
			if _, e := s.Complete(context.Background(), code, state, issuer); e == nil {
				t.Fatal("accepted invalid callback")
			}
			if *calls != 0 {
				t.Fatal("sent code before validating callback")
			}
		})
	}
}
func TestBrowserLoginRejectsInvalidClaims(t *testing.T) {
	for _, scenario := range []string{"issuer", "audience", "expiry", "binding", "tenant"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := setup(t, func(c map[string]any) {
				switch scenario {
				case "issuer":
					c["iss"] = "https://attacker.test"
				case "audience":
					c["aud"] = "wrong"
				case "expiry":
					c["exp"] = 1
				case "binding":
					c["cnf"] = map[string]string{"jkt": "wrong"}
				case "tenant":
					c["tenant_uuid"] = "not-a-uuid"
				}
			})
			if _, e := s.Complete(context.Background(), "valid-code", s.state, s.meta.Issuer); e == nil {
				t.Fatal("accepted invalid claims")
			}
		})
	}
}

func TestRealmLoginURL(t *testing.T) {
	for _, tc := range []struct {
		url   string
		valid bool
	}{
		{"/realms/acme-123/account/login?email=user%40example.test", true},
		{AuthBase + "/realms/acme-123/account/login?email=user%40example.test", true},
		{"https://attacker.test/realms/acme-123/account/login", false},
		{"//attacker.test/realms/acme-123/account/login", false},
		{"http://auth.dev.wendy.sh/realms/acme-123/account/login", false},
		{"https://user@auth.dev.wendy.sh/realms/acme-123/account/login", false},
		{"realms/acme-123/account/login", false},
		{"/realms//account/login", false},
		{"/realms/%2e%2e/account/login", false},
		{"", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			got, err := issuerFromRealmLoginURL(tc.url)
			if tc.valid {
				if err != nil || got != AuthBase+"/realms/acme-123" {
					t.Fatalf("issuer=%q err=%v", got, err)
				}
			} else if err == nil {
				t.Fatalf("accepted %q", tc.url)
			}
		})
	}
}
func TestBeginWithEmailRealmLookup(t *testing.T) {
	s, _ := setup(t, nil)
	existing := s.Client
	s.Client = doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == AuthBase+"/api/login/realm" {
			if r.Method != "POST" {
				t.Fatal("lookup must POST")
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["email"] != "user@example.test" {
				t.Fatal("incorrect email request")
			}
			return reply(200, map[string]string{"loginURL": "/realms/system/account/login?email=user%40example.test"}), nil
		}
		return existing.Do(r)
	})
	authorize, err := s.Begin(context.Background(), "user@example.test", "http://localhost:5173/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(authorize, AuthBase+"/realms/system/authorize?") {
		t.Fatal("wrong authorize realm")
	}
}

type profileCloudServer struct {
	cloudpbv2.UnimplementedOrganizationServiceServer
	cloudpbv2.UnimplementedAssetServiceServer
	wrongTenant bool
}

func (s profileCloudServer) GetOrganization(_ context.Context, r *cloudpbv2.GetOrganizationRequest) (*cloudpbv2.Organization, error) {
	id := r.Id
	if s.wrongTenant {
		id = "other-tenant"
	}
	return &cloudpbv2.Organization{Id: id, Name: "Research lab"}, nil
}
func (s profileCloudServer) ListAssets(request *cloudpbv2.ListAssetsRequest, _ grpc.ServerStreamingServer[cloudpbv2.ListAssetsResponse]) error {
	if !request.GetOnlineOnly() {
		return fmt.Errorf("expected online-only discovery")
	}
	return nil
}
func TestCloudProfileOrganization(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(fmt.Sprint(wrong), func(t *testing.T) {
			s, _ := setup(t, nil)
			p, err := s.Complete(context.Background(), "valid-code", s.state, s.meta.Issuer)
			if err != nil {
				t.Fatal(err)
			}
			if p.Email != "operator@example.test" {
				t.Fatal("email must come from UserInfo when absent in token")
			}
			listener := bufconn.Listen(1024 * 1024)
			server := grpc.NewServer()
			defer server.Stop()
			defer listener.Close()
			svc := profileCloudServer{wrongTenant: wrong}
			cloudpbv2.RegisterOrganizationServiceServer(server, svc)
			cloudpbv2.RegisterAssetServiceServer(server, svc)
			go server.Serve(listener)
			_, err = s.Discover(context.Background(), func(ctx context.Context, _ string) (net.Conn, error) {
				conn, err := listener.DialContext(ctx)
				if err == nil {
					context.AfterFunc(ctx, func() { conn.Close() })
				}
				return conn, err
			})
			if err != nil {
				t.Fatal(err)
			}
			p = s.Profile()
			if wrong {
				if p.Organization != "" || p.ProfileWarning == "" {
					t.Fatal("accepted another tenant's name")
				}
			} else if p.Organization != "Research lab" || p.ProfileWarning != "" {
				t.Fatalf("incorrect profile: %+v", p)
			}
		})
	}
}
func TestUserInfoSubjectMismatch(t *testing.T) {
	s, _ := setup(t, nil)
	base := s.Client
	s.Client = doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/userinfo") {
			return reply(200, map[string]string{"sub": "other-operator", "email": "wrong@example.test"}), nil
		}
		return base.Do(r)
	})
	p, err := s.Complete(context.Background(), "valid-code", s.state, s.meta.Issuer)
	if err != nil {
		t.Fatal(err)
	}
	if p.Email != "" || p.ProfileWarning == "" {
		t.Fatal("accepted another operator's profile")
	}
}

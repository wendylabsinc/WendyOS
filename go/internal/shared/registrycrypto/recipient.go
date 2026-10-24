package registrycrypto

import (
	"bytes"
	"crypto/hpke"
	"crypto/mldsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

const (
	registrationPayloadType = "wendycloud.v2.RegisterEncryptionKeyRequest"
	registerDeviceKey       = "wendycloud.v2.KeyDirectoryService/RegisterDeviceEncryptionKey"
	registerUserKey         = "wendycloud.v2.KeyDirectoryService/RegisterUserEncryptionKey"
	maxRegistrationJWS      = 32768
)

// Registration is the RegisterEncryptionKeyRequest that registers pub for
// organizationID, the one shape the CLI and wendy-web sign.
func Registration(organizationID string, pub hpke.PublicKey) *cloudpbv2.RegisterEncryptionKeyRequest {
	return &cloudpbv2.RegisterEncryptionKeyRequest{OrganizationId: organizationID, Suite: cloudpbv2.KemSuite_KEM_SUITE_MLKEM768_X25519, PublicKey: pub.Bytes()}
}

// ErrRegistration is every refusal of a served key registration. Cloud
// serves registrations and is untrusted, so the reason never matters to the
// caller: the recipient is not wrapped to.
var ErrRegistration = errors.New("recipient key registration does not verify")

// VerifyRegistration checks a RecipientKey's registration as the client that
// will wrap to it and returns the public key it registers, the only key the
// caller may wrap to for principal (its key id is KeyID of it). It checks the
// JWS against its x5c leaf, that leaf through chainDER (intermediates up to
// the tenant CA) to the pinned platform root at the JWS's signed iat (never
// in the future), the leaf identity against principal and its tenant
// against organizationID, the operation against the principal kind, the body
// digest, and the signed payload's organization_id and suite. Only ML-DSA-65
// is accepted from devices (the tier A/B device-signer profile; tier C has no
// wrap suite yet), ML-DSA-65 or -87 from operators. Revocation is cloud's:
// it verified the registration when it was made and stops serving revoked
// keys.
func VerifyRegistration(reg *cloudpbv2.SignedRequest, chainDER [][]byte, root *x509.Certificate, principal, organizationID string) ([]byte, error) {
	payload, err := verifyRegistration(reg, chainDER, root, principal, organizationID, time.Now())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRegistration, err)
	}
	publicKey, err := registeredKey(payload, organizationID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRegistration, err)
	}
	return publicKey, nil
}

// registeredKey decodes a RegisterEncryptionKeyRequest and checks it. The
// payload must be exactly the encoding of what it decodes to: a repeated
// field, which two parsers could resolve to different keys, or any other
// non-canonical encoding is refused. Every producer (CLI, wendy-web, agent)
// encodes it with Go's proto.Marshal.
func registeredKey(payload []byte, organizationID string) ([]byte, error) {
	var req cloudpbv2.RegisterEncryptionKeyRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	// A field of the wrong wire type lands in unknown fields and re-marshals
	// byte-identically, so the round trip alone does not catch it.
	if len(req.ProtoReflect().GetUnknown()) != 0 {
		return nil, errors.New("registration payload carries unknown fields")
	}
	if again, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&req); err != nil || !bytes.Equal(again, payload) {
		return nil, errors.New("registration payload is not canonically encoded")
	}
	if req.GetOrganizationId() != organizationID {
		return nil, errors.New("registration is for another organization")
	}
	if req.GetSuite() != cloudpbv2.KemSuite_KEM_SUITE_MLKEM768_X25519 {
		return nil, errors.New("registration names an unknown suite")
	}
	if _, err := KEM.NewPublicKey(req.GetPublicKey()); err != nil {
		return nil, err
	}
	return req.GetPublicKey(), nil
}

func verifyRegistration(reg *cloudpbv2.SignedRequest, chainDER [][]byte, root *x509.Certificate, principal, organizationID string, now time.Time) ([]byte, error) {
	if reg == nil || root == nil {
		return nil, errors.New("no registration or pinned root")
	}
	if reg.GetPayloadType() != registrationPayloadType {
		return nil, errors.New("wrong payload type")
	}
	id, err := certs.ParsePrincipal(principal)
	if err != nil || id.TenantUUID != organizationID {
		return nil, errors.New("principal is not in the organization")
	}
	operation := map[string]string{certs.EntityAsset: registerDeviceKey, certs.EntityUser: registerUserKey}[id.EntityType]
	if operation == "" {
		return nil, errors.New("principal is neither a device nor a user")
	}

	compact := string(reg.GetSignature())
	parts := strings.Split(compact, ".")
	if len(compact) > maxRegistrationJWS || len(parts) != 3 {
		return nil, errors.New("malformed JWS")
	}
	rawHeader, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var header struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	dec := json.NewDecoder(bytes.NewReader(rawHeader))
	dec.DisallowUnknownFields() // kid and crit are refused here
	if err := dec.Decode(&header); err != nil || len(header.X5C) != 1 {
		return nil, errors.New("header must carry alg and exactly one x5c leaf")
	}
	leafDER, err := base64.StdEncoding.Strict().DecodeString(header.X5C[0])
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return nil, err
	}
	if san, ok := certs.TenantPrincipalFromCert(leaf); !ok || san != principal {
		return nil, errors.New("leaf identity is not the principal")
	}

	// The algorithm is fixed by the signer, never by the header.
	pub, ok := leaf.PublicKey.(*mldsa.PublicKey)
	if !ok {
		return nil, errors.New("leaf key is not ML-DSA")
	}
	switch {
	case header.Alg == "ML-DSA-65" && pub.Parameters() == mldsa.MLDSA65():
	case header.Alg == "ML-DSA-87" && pub.Parameters() == mldsa.MLDSA87() && id.EntityType == certs.EntityUser:
	default:
		return nil, errors.New("signature algorithm not allowed for this signer")
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	if err := mldsa.Verify(pub, []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
		return nil, errors.New("signature does not verify")
	}

	rawClaims, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims struct {
		Operation string `json:"operation"`
		Digest    string `json:"body_sha256"`
		Iat       int64  `json:"iat"`
		Target    struct {
			Tenant   string `json:"tenant"`
			Resource string `json:"resource"`
		} `json:"target"`
	}
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(reg.GetPayload())
	if claims.Operation != operation || claims.Target.Tenant != organizationID || claims.Digest != base64.RawURLEncoding.EncodeToString(sum[:]) {
		return nil, errors.New("claims do not bind this registration")
	}
	if id.EntityType == certs.EntityAsset && claims.Target.Resource != principal {
		return nil, errors.New("device registration names another device")
	}
	if claims.Iat > now.Add(time.Minute).Unix() {
		return nil, errors.New("registration is signed in the future")
	}

	// The issuing CA must be the ML-DSA Device Identity CA (or operator CA):
	// a P-256 leaf under it, or any leaf under the Lite CA, is refused above
	// or here.
	if len(chainDER) == 0 {
		return nil, errors.New("no chain to the pinned root")
	}
	chain := make([]*x509.Certificate, 0, len(chainDER))
	for _, der := range chainDER {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		chain = append(chain, c)
	}
	if _, ok := chain[0].PublicKey.(*mldsa.PublicKey); !ok || !bytes.Equal(chain[0].RawSubject, leaf.RawIssuer) {
		return nil, errors.New("issuing CA is not an ML-DSA CA")
	}
	at := time.Unix(claims.Iat, 0)
	if err := certs.VerifyPeerCertificateChain(leaf, chain, []*x509.Certificate{root}, x509.ExtKeyUsageClientAuth, at, at); err != nil {
		return nil, err
	}
	return reg.GetPayload(), nil
}

// RecipientCustodian is RecipientKind RECIPIENT_KIND_CUSTODIAN: the Wendy
// break-glass key every release and deployment is wrapped to (Recipient empty,
// RecipientKeyID = CustodianKey's key id).
const RecipientCustodian byte = 3

// ErrNoCustodian refuses a custodian wrap in an environment with no pinned
// custodian key. Prod has none until its ceremony (Q9) exists; there is no
// fallback key.
var ErrNoCustodian = errors.New("no registry custodian key is pinned for this environment")

// devCustodianPublicKey is the dev custodian key (Cloud KMS KEM_XWING,
// registry-custodian-dev version 1), xwing raw bytes in hex (WDY-4007).
const devCustodianPublicKey = "a912b0fd069befc54d265c83b555554de42d8b9714820155fce063a2454a47235a3223c19c954ee4e9b1d5861ffe5306ff20839311b0d1d780960512ad865622971d3591c5347b826d954918a04587004830d1890835c73b108848f77090a309b6e551a91b6ebd0aa1b6b4c68c3b3285721feb91311ca263c4cc5e559a4af4238bd56c84fcb809bef5071e849d14568fa57ba72a630d2826486aa88217a8b99f6680a2473a3881bf69650cbcd953dd525265d3001630470de8014746cd3c49a7e4736d61a399cf3b749cc59ccd792bb317769523271b9a2796498dd409b672f02f2f0bc4ae09855afcb642796219a7bf05826eb6715bc29530c10061be4ba12d016d7b053d7643aa3687733e31b9e8a643de040e1fc77140f808b47b61885a6df9201525245e5247899ddb81605496f7c26b4ee04adcfc3fb49146cf874639a77d7b2caeb072019c79150ebb30701908161480d10a85245955a5c461bef52926fa52a2e513a9838481e26e0ab2341e6373a0f03af42b2dc6029a2e694ddcda175c5794a07371a2123caeab6a68327216a87b868345daeacd9cb1306b72afe5aa3747183ea1946a2b7ca55a9c55bb9acf11824c7bf3321384333c01bef1d04f62bc10bf89a5b7f736d9768b8ac455ae8249aeb91f614932c01a0bca63779c122549a4c39ff900a83a5b4d491cccd84aab0b3b6f9144f2824e14682c821b50c0c1bf3fd82935801b3e171985e63d8409179045ab99246fd5f8aa437038f800cf1550702f26b57cd530a8fb7007dbc2b5a17d69d75f3b3a370050a29c39a531b80fd7681103964f9178017df7841818c68efab343471721115a9de99c895233e4740befa6300d93545dd75b4828cd12b6bc761108d3ea5fd5da779a176bd0d94a26b89019425693773a71abad8e61496a2a8c48e7641e098eb7088157837db327a33930b8cb144a5b2870aab46c01458811122081b0156cfb278113631257c2e78a30518572544491d1935704037907450b2700198b2c173d59177940b49f6619fe306ecfe79b06ea80b542981a05b3c5191ff0f4c0501cacbdb53fa806be7c4680b8146f0cd660bb593182c9b2c55b83e1365cf381688a0ba14de0145f444a73239985660323f8c514592b0487c18f1951652c72d6e6b1ceca7f618603902949421543fce5c0cb628a4e731370e07e10f56ab4f374b2cc54ea48356fc9c15134046da703ed29cc8f30647aa455f1768a5cf068d4084e8e2cccf68652aa547829921ba8960329e3b87ec28c7eb335c62823853274dda29f2977525ec91f6802ae68079edc7a6f2d9936eaa77f1c4cb4475400d196c73f27089bfb49b4611c0229254fd73696a557ca89b908b36265973cc2d1222640a5e51886577a79c5344b92ca96d8295bb371895b78493a4cc93290913d7bcf4b361608783e86372a43b55ff9821f90cc0a2b2b132ee639a7f170f1744055531fa73158140795a38968db5894bff7cbea7062cdc0aa4af4860be49705bc10e67a5c4e567f02455728fb4ad75c3f8b8142a502496f76902ce1a57c332454bc1ca1fa5c7d757ea2720c9016717cf5b696626fd7a6c4e9b4a5cb3aaedfb34691431df80c316ce96a8fc4846544a633e904debcd8cfc1af8e5bbb57c4f6a08188d3a79295a2adbe301e89647f11b292e0be36933308c3db4f0eee32fe43ef976568b4175fdeb9397ba76a0be8a16c"

// CustodianKey returns the pinned custodian public key and its key id for the
// dev or prod environment.
func CustodianKey(dev bool) (publicKey []byte, keyID string, err error) {
	if !dev {
		return nil, "", ErrNoCustodian
	}
	pub, err := hex.DecodeString(devCustodianPublicKey)
	if err != nil {
		return nil, "", err
	}
	return pub, KeyID(pub), nil
}

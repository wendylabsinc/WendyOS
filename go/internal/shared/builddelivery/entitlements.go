package builddelivery

import (
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// The deployed pki-core OID config uses PEN.1, encoding a DER SEQUENCE OF
// UTF8String. Match its target grammar and 2048-byte issuance budget. Device
// scope must be resolved into the certificate by Cloud; unsigned site/tag
// membership cannot expand authority on an offline target.
var entitlementOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 1}

func authorizeEntitlements(leaf *x509.Certificate, target meshid.Device) error {
	var encoded []byte
	for _, ext := range leaf.Extensions {
		if ext.Id.Equal(entitlementOID) {
			if encoded != nil {
				return errors.New("duplicate entitlement extension")
			}
			encoded = ext.Value
		}
	}
	if len(encoded) == 0 || len(encoded) > 2048 {
		return errors.New("operator certificate requires bounded embedded deployment entitlements")
	}
	var values []asn1.RawValue
	rest, err := asn1.Unmarshal(encoded, &values)
	if err != nil || len(rest) != 0 {
		return errors.New("malformed operator entitlements")
	}
	allowed := false
	for _, v := range values {
		if v.Class != asn1.ClassUniversal || v.Tag != asn1.TagUTF8String || v.IsCompound || !utf8.Valid(v.Bytes) {
			return errors.New("invalid entitlement string")
		}
		text := string(v.Bytes)
		body, ok := strings.CutPrefix(text, "entitlement:")
		if !ok {
			return errors.New("invalid entitlement prefix")
		}
		category, body, ok := strings.Cut(body, ":")
		if !ok || category == "" {
			return errors.New("invalid entitlement category")
		}
		permission, body, ok := strings.Cut(body, ":")
		if !ok || permission == "" {
			return errors.New("invalid entitlement permission")
		}
		scope, effect := "", body
		if at := strings.LastIndexByte(body, ':'); at >= 0 {
			scope, effect = body[:at], body[at+1:]
			if scope != "all" && scope != "*" {
				kind, value, found := strings.Cut(scope, ":")
				if !found || (kind != "device" && kind != "site" && kind != "tag") || value == "" || strings.Contains(value, "*") || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0 {
					return errors.New("invalid entitlement target")
				}
			}
		}
		if effect != "allow" && effect != "deny" {
			return errors.New("invalid entitlement effect")
		}
		if category != "deployment" || (permission != "create" && permission != "*") {
			continue
		}
		matches := scope == "" || scope == "all" || scope == "*" || scope == "device:"+target.UUID().String()
		// Unknown site/tag membership may narrow a grant (deny wins), never widen it.
		if effect == "deny" && (matches || strings.HasPrefix(scope, "site:") || strings.HasPrefix(scope, "tag:")) {
			return errors.New("operator deployment entitlement denied")
		}
		if effect == "allow" && matches {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("operator lacks deployment:create for the target")
	}
	return nil
}

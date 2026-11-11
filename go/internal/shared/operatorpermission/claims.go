// Package operatorpermission evaluates Cloud-issued operator certificate claims.
// Callers must verify the certificate chain before using this package.
package operatorpermission

import (
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

var OID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 1}

const MaxEncodedBytes = 2048

type Claim struct{ Category, Permission, Target, Effect string }

// Parse rejects an invalid extension in its entirety. In particular, an unknown
// target must not silently remove a deny from an otherwise usable certificate.
func Parse(leaf *x509.Certificate) ([]Claim, bool, error) {
	if leaf == nil {
		return nil, false, errors.New("certificate required")
	}
	var value []byte
	found := false
	for _, ext := range leaf.Extensions {
		if ext.Id.Equal(OID) {
			if found {
				return nil, true, errors.New("duplicate entitlement extension")
			}
			found, value = true, ext.Value
		}
	}
	if !found {
		return nil, false, nil
	}
	if len(value) > MaxEncodedBytes {
		return nil, true, errors.New("entitlement extension exceeds budget")
	}
	var seq asn1.RawValue
	rest, err := asn1.Unmarshal(value, &seq)
	if err != nil || len(rest) != 0 || seq.Class != 0 || seq.Tag != 16 || !seq.IsCompound {
		return nil, true, errors.New("invalid entitlement sequence")
	}
	var claims []Claim
	for data := seq.Bytes; len(data) > 0; {
		var item asn1.RawValue
		data, err = asn1.Unmarshal(data, &item)
		if err != nil || item.Class != 0 || item.Tag != 12 || item.IsCompound || !utf8.Valid(item.Bytes) {
			return nil, true, errors.New("entitlement must be a UTF8String")
		}
		claim, err := ParseClaim(string(item.Bytes))
		if err != nil {
			return nil, true, err
		}
		claims = append(claims, claim)
	}
	return claims, true, nil
}

func ParseClaim(s string) (Claim, error) {
	bad := func() (Claim, error) { return Claim{}, fmt.Errorf("invalid entitlement grammar") }
	if strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) || unicode.IsSpace(r) }) >= 0 {
		return bad()
	}
	parts := strings.Split(s, ":")
	if len(parts) < 4 || parts[0] != "entitlement" {
		return bad()
	}
	c := Claim{Category: parts[1], Permission: parts[2], Effect: parts[len(parts)-1]}
	if c.Category == "" || c.Permission == "" || strings.Contains(c.Category, "*") || (strings.Contains(c.Permission, "*") && c.Permission != "*") || (c.Effect != "allow" && c.Effect != "deny") {
		return bad()
	}
	if len(parts) > 4 {
		c.Target = strings.Join(parts[3:len(parts)-1], ":")
	}
	switch c.Target {
	case "", "all", "*":
		c.Target = "all"
	default:
		kind, value, ok := strings.Cut(c.Target, ":")
		if !ok || value == "" || strings.Contains(value, "*") || (kind != "device" && kind != "site" && kind != "tag") {
			return bad()
		}
	}
	return c, nil
}

// Allows supports exact device targets and all-device grants. Site/tag claims
// need authenticated membership metadata that this agent does not yet retain;
// a relevant unresolved deny fails closed, and an unresolved allow grants nothing.
func Allows(claims []Claim, permission, deviceUUID string) bool {
	category, verb, ok := strings.Cut(permission, ":")
	if !ok {
		return false
	}
	allowed := false
	for _, c := range claims {
		if c.Category != category || (c.Permission != verb && c.Permission != "*") {
			continue
		}
		match := c.Target == "all" || (deviceUUID != "" && c.Target == "device:"+deviceUUID)
		unresolved := strings.HasPrefix(c.Target, "site:") || strings.HasPrefix(c.Target, "tag:")
		if c.Effect == "deny" && (match || unresolved) {
			return false
		}
		if c.Effect == "allow" && match {
			allowed = true
		}
	}
	return allowed
}

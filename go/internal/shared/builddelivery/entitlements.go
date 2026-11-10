package builddelivery

import (
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Match pki-core's certificate extension: a DER SEQUENCE OF UTF8String,
// bounded by its 2048-byte issuance budget.
var entitlementOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 1}

type entitlementEffect uint8

const (
	entitlementAllow entitlementEffect = iota + 1
	entitlementDeny
)

type entitlementTargetKind uint8

const (
	targetAll entitlementTargetKind = iota
	targetDevice
	targetSite
	targetTag
)

type entitlementTarget struct {
	kind  entitlementTargetKind
	value string
}

func (t entitlementTarget) matchesDevice(device string) bool {
	return t.kind == targetAll || (t.kind == targetDevice && t.value == device)
}

// Site/tag membership cannot be established from an offline certificate.
func (t entitlementTarget) requiresMembership() bool {
	return t.kind == targetSite || t.kind == targetTag
}

// Category and permission are open names from the PKI permission catalog.
// Effect and target have a fixed grammar and are decoded into typed values.
type entitlement struct {
	category   string
	permission string
	effect     entitlementEffect
	target     entitlementTarget
}

func certificateEntitlements(leaf *x509.Certificate) ([]entitlement, error) {
	var encoded []byte
	found := false
	for _, ext := range leaf.Extensions {
		if ext.Id.Equal(entitlementOID) {
			if found {
				return nil, errors.New("duplicate entitlement extension")
			}
			encoded, found = ext.Value, true
		}
	}
	if len(encoded) == 0 || len(encoded) > 2048 {
		return nil, errors.New("operator certificate requires bounded embedded deployment entitlements")
	}
	var values []asn1.RawValue
	rest, err := asn1.Unmarshal(encoded, &values)
	if err != nil || len(rest) != 0 {
		return nil, errors.New("malformed operator entitlements")
	}
	rules := make([]entitlement, 0, len(values))
	for _, v := range values {
		if v.Class != asn1.ClassUniversal || v.Tag != asn1.TagUTF8String || v.IsCompound {
			return nil, errors.New("invalid entitlement string")
		}
		rule, err := parseEntitlement(string(v.Bytes))
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// The existing PKI grammar is entitlement:category:permission[:target]:effect.
// Effect is the last segment so target values such as tag:env:prod retain ':'.
// An absent target, "all" and "*" all decode to targetAll.
func parseEntitlement(text string) (entitlement, error) {
	var rule entitlement
	if !utf8.ValidString(text) {
		return rule, errors.New("invalid entitlement UTF-8")
	}
	body, ok := strings.CutPrefix(text, "entitlement:")
	if !ok {
		return rule, errors.New("invalid entitlement prefix")
	}
	rule.category, body, ok = strings.Cut(body, ":")
	if !ok || rule.category == "" {
		return rule, errors.New("invalid entitlement category")
	}
	rule.permission, body, ok = strings.Cut(body, ":")
	if !ok || rule.permission == "" {
		return rule, errors.New("invalid entitlement permission")
	}
	effect := body
	if at := strings.LastIndexByte(body, ':'); at >= 0 {
		var err error
		rule.target, err = parseEntitlementTarget(body[:at])
		if err != nil {
			return rule, err
		}
		effect = body[at+1:]
	}
	switch effect {
	case "allow":
		rule.effect = entitlementAllow
	case "deny":
		rule.effect = entitlementDeny
	default:
		return rule, errors.New("invalid entitlement effect")
	}
	return rule, nil
}

func parseEntitlementTarget(text string) (entitlementTarget, error) {
	var target entitlementTarget
	if text == "all" || text == "*" {
		return target, nil
	}
	kind, value, found := strings.Cut(text, ":")
	if !found || value == "" || strings.Contains(value, "*") || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0 {
		return target, errors.New("invalid entitlement target")
	}
	switch kind {
	case "device":
		target.kind = targetDevice
	case "site":
		target.kind = targetSite
	case "tag":
		target.kind = targetTag
	default:
		return target, errors.New("invalid entitlement target kind")
	}
	target.value = value
	return target, nil
}

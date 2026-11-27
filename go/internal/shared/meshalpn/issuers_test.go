package meshalpn

import "testing"

func TestNormalBinaryPrefixAndCachedInventory(t *testing.T) {
	const protocol = "wendy-app-mesh-tls/1"
	var chain []Fingerprint
	for i := 0; i < 3; i++ {
		var hash Fingerprint
		hash[0] = byte(i + 1)
		chain = append(chain, hash)
	}
	var cached []Fingerprint
	for i := 0; i < 12; i++ {
		var hash Fingerprint
		hash[0] = byte(i + 128)
		cached = append(cached, hash)
	}
	offered := Offer(protocol, chain, cached)
	base, full := Decode(offered[0])
	if base != protocol || len(full) != 3 || len(offered[0]) != len(protocol)+5+3*16 {
		t.Fatal("binary CA fingerprints changed framing")
	}
	for i := range chain {
		if full[i] != chain[i] {
			t.Fatal("root-first CA ordering changed")
		}
	}
	selected := Select([]string{protocol}, offered, func(hash Fingerprint) bool { return hash == chain[0] || hash == chain[1] })
	app, prefix := Decode(selected)
	if app != protocol || len(prefix) != 2 || Application(selected) != protocol {
		t.Fatal("partial shared ancestry was not acknowledged")
	}
	known := Offered(offered, protocol)
	for _, hash := range append(chain, cached...) {
		if !known[hash] {
			t.Fatal("available cached CA was not advertised")
		}
	}
	for _, name := range offered {
		if len(name) > 255 {
			t.Fatal("ALPN name exceeded TLS bound")
		}
	}
	if ordinary := Select([]string{protocol}, offered, func(Fingerprint) bool { return false }); ordinary != protocol {
		t.Fatal("ordinary full-chain fallback lost its application protocol")
	}
}

func TestLeafOnlyOfferKeepsLegacyFallback(t *testing.T) {
	const base = "wendy-local-mesh-ble/2"
	issuer := Fingerprint{0x8a, 0x75}
	for _, kind := range []byte{classicalClient, regularClient} {
		offer := leafOffer(base, kind, issuer)
		application, decodedKind, decodedIssuer := decodeLeafOffer(offer)
		if application != base || decodedKind != kind || decodedIssuer != issuer ||
			Application(offer) != base || len(offer) != len(base)+5+1+16 {
			t.Fatalf("invalid leaf-only offer for role %d", kind)
		}
		if selected := Select([]string{base}, []string{offer, base}, func(Fingerprint) bool { return true }); selected != base {
			t.Fatal("legacy prefix negotiation treated a leaf-only offer as a prefix")
		}
		if Offered([]string{offer}, base)[issuer] {
			t.Fatal("leaf-only offer was confused with cache inventory")
		}
	}
	if _, kind, _ := decodeLeafOffer(leafOffer(base, 0x7f, issuer)); kind != 0 {
		t.Fatal("unknown leaf-only role was accepted")
	}
}

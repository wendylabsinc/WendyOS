package builddelivery

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func FuzzDeliveryAdmission(f *testing.F) {
	fixture := newFixtureAt(f, time.Unix(1790000000, 0))
	compact := fixture.sign(f, fixture.claims)
	if _, err := Admit(compact, fixture.roots, fixture.target, fixture.now); err != nil {
		f.Fatal(err)
	}
	parts := strings.Split(compact, ".")
	payload, err := decode(parts[1])
	if err != nil {
		f.Fatal(err)
	}
	signature, err := decode(parts[2])
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte("other"), uint16(0), byte(0))
	f.Add([]byte{0xff}, uint16(100), byte(1))
	f.Add([]byte("reference"), uint16(0), byte(2))
	f.Add([]byte(`{"v":1,"v":1}`), uint16(0), byte(3))
	f.Fuzz(func(t *testing.T, input []byte, offset uint16, mode byte) {
		if len(input) > MaxContractBytes {
			return
		}
		mutated := append([]string(nil), parts...)
		switch mode % 4 {
		case 0:
			// A changed, otherwise valid reference must not retain authority.
			claims := fixture.claims
			claims.Reference = "lab/other:latest"
			if len(input) > 0 {
				claims.MaxBytes = 1 + int64(offset)
			}
			raw, err := json.Marshal(claims)
			if err != nil {
				t.Fatal(err)
			}
			mutated[1] = encode(raw)
		case 1:
			bad := bytes.Clone(signature)
			bad[int(offset)%len(bad)] ^= 1 << (mode / 4 % 8)
			mutated[2] = encode(bad)
		case 2:
			// Test the strict parser independently: an invalid signature must
			// not hide a regression in duplicate-key rejection.
			var members map[string]json.RawMessage
			if err := json.Unmarshal(payload, &members); err != nil {
				t.Fatal(err)
			}
			keys := []string{"v", "jti", "iss", "aud", "reference", "operations", "max_bytes"}
			key := keys[int(offset)%len(keys)]
			keyJSON, _ := json.Marshal(key)
			duplicate := append([]byte("{"+string(keyJSON)+":"+string(members[key])+","), payload[1:]...)
			var claims Claims
			if err := strictJSON(duplicate, &claims); err == nil {
				t.Fatal("accepted duplicate claim")
			}
			mutated[1] = encode(duplicate)
		case 3:
			// Arbitrary JSON exercises depth, nulls, unknown members and types.
			var claims Claims
			_ = strictJSON(input, &claims)
			mutated[1] = encode(input)
			if bytes.Equal(input, payload) {
				return
			}
		}
		if _, err := Admit(strings.Join(mutated, "."), fixture.roots, fixture.target, fixture.now); err == nil {
			t.Fatal("tampered delivery contract retained authority")
		}
	})
}

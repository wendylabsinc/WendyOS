package bleprovider

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAdvertisementConformanceVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/advertisement-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		NetworkName string `json:"network_name"`
		Device      string `json:"device_uuid"`
		PSM         uint16 `json:"psm"`
		Binary      string `json:"binary_hex"`
		Name        string `json:"local_name"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		t.Run(v.NetworkName, func(t *testing.T) {
			advert, err := NewAdvertisement(uuid.MustParse(v.Device), v.NetworkName, v.PSM)
			if err != nil {
				t.Fatal(err)
			}
			binary, err := advert.MarshalBinary()
			if err != nil || hex.EncodeToString(binary) != v.Binary {
				t.Fatalf("binary %x: %v", binary, err)
			}
			name, err := advert.MarshalLocalName()
			if err != nil || name != v.Name {
				t.Fatalf("name %q, want %q: %v", name, v.Name, err)
			}
			for _, parse := range []func() (Advertisement, error){func() (Advertisement, error) { return ParseAdvertisement(binary, v.NetworkName) }, func() (Advertisement, error) { return ParseLocalName(name, v.NetworkName) }} {
				got, err := parse()
				if err != nil || got != advert {
					t.Fatalf("decoded %+v: %v", got, err)
				}
			}
			if len(name) != 13 || 3+2+16+len(binary) != 31 {
				t.Fatal("wire budget changed")
			}
		})
	}
}

func TestLocalNameAll80BitBoundaries(t *testing.T) {
	values := []string{"0", "1", "18446744073709551615", "18446744073709551616", "36893488147419103231", "1208925819614629174706175"}
	for _, decimal := range values {
		n, _ := new(big.Int).SetString(decimal, 10)
		raw := n.FillBytes(make([]byte, AdvertisementSize))
		name, err := encodeLocalName(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeLocalName(name)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("80-bit boundary %s: %x %v", decimal, got, err)
		}
	}
	if name, _ := encodeLocalName(make([]byte, AdvertisementSize)); name != strings.Repeat("!", LocalNameSize) {
		t.Fatalf("zero padding: %q", name)
	}
	for _, name := range []string{"", "!!!!!!!!!", "!!!!!!!!!!!", "~~~~~~~~~~", " !!!!!!!!!", "\x7f!!!!!!!!!", "é!!!!!!!!"} {
		if _, err := decodeLocalName(name); err == nil {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
	if _, err := encodeLocalName(append([]byte{2}, make([]byte, 8)...)); err == nil {
		t.Fatal("encoded wrong-length value")
	}
}

func TestAdvertisementStrictVersionsAndFields(t *testing.T) {
	a, _ := NewAdvertisement(lowDevice, "field", 0x81)
	raw, _ := a.MarshalBinary()
	for _, version := range []byte{0, 2, 0x80, 0xff} {
		bad := bytes.Clone(raw)
		bad[0] = version
		if _, err := ParseAdvertisement(bad, "field"); err == nil {
			t.Fatalf("accepted version %d", version)
		}
		a.Version = version
		if _, err := a.MarshalBinary(); err == nil {
			t.Fatal("marshaled unknown version")
		}
		if _, err := a.MarshalLocalName(); err == nil {
			t.Fatal("marshaled unknown name version")
		}
		if version == 2 {
			name, _ := encodeLocalName(bad)
			if _, err := ParseLocalName(name, "field"); err == nil {
				t.Fatal("accepted text version 2")
			}
		}
	}
	name, _ := encodeLocalName(raw)
	if _, err := ParseLocalName(name, "different mesh"); err == nil {
		t.Fatal("accepted wrong mesh")
	}
	for _, extension := range [][]byte{{0}, {0x80, 1, 2, 3, 4}} {
		got, err := ParseAdvertisement(append(bytes.Clone(raw), extension...), "field")
		want, _ := ParseAdvertisement(raw, "field")
		if err != nil || got != want {
			t.Fatalf("optional service-data extension changed v1 prefix: %+v %v", got, err)
		}
	}
	if _, err := encodeLocalName(append(bytes.Clone(raw), 0)); err == nil {
		t.Fatal("local-name codec accepted an extended binary layout")
	}
	for _, psm := range []uint16{0, 1, 0x7f, 0x100, 0x1001, 0xffff} {
		bad := bytes.Clone(raw)
		bad[2], bad[3] = byte(psm>>8), byte(psm)
		name, _ := encodeLocalName(bad)
		if _, err := ParseLocalName(name, "field"); err == nil {
			t.Fatalf("accepted PSM %x", psm)
		}
	}
}

func TestLocalNameRandomPayloads(t *testing.T) {
	rng := rand.New(rand.NewPCG(22, 41))
	for range 10000 {
		raw := make([]byte, AdvertisementSize)
		raw[0] = byte(rng.Uint32() & 1)
		for i := 1; i < len(raw); i++ {
			raw[i] = byte(rng.Uint32())
		}
		name, err := encodeLocalName(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeLocalName(name)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("roundtrip: %x -> %q -> %x %v", raw, name, got, err)
		}
	}
}

func FuzzAdvertisementBinary(f *testing.F) {
	a, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := a.MarshalBinary()
	f.Add(raw)
	f.Fuzz(func(t *testing.T, raw []byte) {
		a, err := ParseAdvertisement(raw, "field")
		if err != nil {
			return
		}
		got, err := a.MarshalBinary()
		if err != nil || !bytes.Equal(got, raw[:AdvertisementSize]) {
			t.Fatal("accepted inconsistent v1 prefix")
		}
		name, err := a.MarshalLocalName()
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseLocalName(name, "field")
		if err != nil || a != b {
			t.Fatal("representations disagree")
		}
	})
}

func FuzzAdvertisementLocalName(f *testing.F) {
	a, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	name, _ := a.MarshalLocalName()
	f.Add(name)
	f.Add(strings.Repeat("~", 10))
	f.Fuzz(func(t *testing.T, name string) {
		raw, err := decodeLocalName(name)
		if err != nil {
			return
		}
		got, err := encodeLocalName(raw)
		if err != nil || got != name {
			t.Fatal("accepted noncanonical name")
		}
	})
}

func FuzzAdvertisingData(f *testing.F) {
	f.Add([]byte{2, 1, 6})
	f.Add([]byte{3, 9, 'a', 'b'})
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = parseAdvertisingData(data, ServiceUUID(testTenant)) })
}

func TestAdvertisingDataSkipsUnsupportedVersionsAmongV1(t *testing.T) {
	advert, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := advert.MarshalBinary()
	id := testTenant.UUID()
	wire := make([]byte, 16)
	for i := range wire {
		wire[i] = id[15-i]
	}
	field := func(payload []byte) []byte {
		value := append(bytes.Clone(wire), payload...)
		return append([]byte{byte(len(value) + 1), 0x21}, value...)
	}
	known := field(append(bytes.Clone(raw), 0x81, 2, 3))
	for _, version := range []byte{0, 2, 255} {
		future := field([]byte{version})
		for _, report := range [][]byte{append(bytes.Clone(future), known...), append(bytes.Clone(known), future...)} {
			fields, err := parseAdvertisingData(report, ServiceUUID(testTenant))
			if err != nil || !fields.servicePresent {
				t.Fatalf("unsupported version %d hid v1: %+v %v", version, fields, err)
			}
			got, err := ParseAdvertisement(fields.service, "field")
			if err != nil || got != advert {
				t.Fatalf("v1 prefix after unsupported version %d: %+v %v", version, got, err)
			}
		}
		fields, err := parseAdvertisingData(future, ServiceUUID(testTenant))
		if err != nil || fields.servicePresent {
			t.Fatalf("unsupported-only report became v1: %+v %v", fields, err)
		}
		mapped := map[string][]byte{ServiceUUID(testTenant): {version}, strings.ToUpper(ServiceUUID(testTenant)): raw}
		if got, ok := matchingServiceData(mapped, ServiceUUID(testTenant), "field"); !ok || got != advert {
			t.Fatalf("unsupported mapped version %d hid v1: %+v %v", version, got, ok)
		}
	}
	for _, duplicate := range [][]byte{raw, {AdvertisementVersion}} {
		mapped := map[string][]byte{ServiceUUID(testTenant): raw, strings.ToUpper(ServiceUUID(testTenant)): duplicate}
		if _, ok := matchingServiceData(mapped, ServiceUUID(testTenant), "field"); ok {
			t.Fatal("ambiguous or malformed mapped v1 service-data was accepted")
		}
	}
	if _, err := parseAdvertisingData(append(bytes.Clone(known), known...), ServiceUUID(testTenant)); err == nil {
		t.Fatal("two supported service-data entries were accepted")
	}
	if _, err := parseAdvertisingData(field(nil), ServiceUUID(testTenant)); err == nil {
		t.Fatal("missing discovery version was accepted")
	}
}

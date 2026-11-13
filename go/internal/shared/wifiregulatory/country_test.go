package wifiregulatory

import "testing"

func TestCountry(t *testing.T) {
	for _, code := range []string{"gb", "US", "AU", "AX", "BQ"} {
		if _, err := Country(code); err != nil {
			t.Fatalf("%s: %v", code, err)
		}
	}
	for _, code := range []string{"", "00", "ZZ", "USA", "EU"} {
		if _, err := Country(code); err == nil {
			t.Fatalf("accepted non-country %s", code)
		}
	}
}

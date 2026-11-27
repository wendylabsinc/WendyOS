package livepeople

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfiguredRequestTimeoutKeepsClockProtocolBound(t *testing.T) {
	f, api := setupPeople(t)
	for _, ms := range []int64{3000, 5000} {
		c := f.cfg
		c.RequestTimeoutMS = ms
		normalized, err := c.normalized()
		if err != nil || normalized.RequestTimeoutMS != ms {
			t.Fatalf("configured timeout %d refused: %v", ms, err)
		}
		// appclock.New rejects any MaxRoundTrip above the protocol's one
		// second limit. Session construction must keep that separate bound.
		s, err := New(c, api, f.infer)
		if err != nil {
			t.Fatalf("clock incompatible with timeout %d: %v", ms, err)
		}
		_, err = s.clock.Observe(context.Background())
		s.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	c := f.cfg
	c.RequestTimeoutMS = 5001
	if _, err := c.normalized(); err == nil {
		t.Fatal("accepted timeout above five seconds")
	}
	if s, err := New(c, api, f.infer); err == nil {
		s.Close()
		t.Fatal("session accepted oversized timeout")
	}
}

func TestConfigDefaultsAndOwnedSourcePins(t *testing.T) {
	f, _ := setupPeople(t)
	c, err := f.cfg.normalized()
	if err != nil || c.RequestTimeoutMS != 1000 || c.PollIntervalMS != 250 || c.RetainForMS != 60000 || c.ClockMaxWidthMS != 5000 || c.Sources[0].Device != c.Device {
		t.Fatalf("configuration defaults: %+v %v", c, err)
	}
	c.Sources[0].SensorID = "changed"
	c.RootsDER[0][0] ^= 1
	if f.cfg.Sources[0].SensorID != "people-front" || c.RootsDER[0][0] == f.cfg.RootsDER[0][0] {
		t.Fatal("normalized config aliases caller input")
	}
	c = f.cfg
	c.Sources = append([]Source(nil), c.Sources...)
	c.Sources[0].Device = "spiffe://wendy.sh/tenant/33333333-3333-4333-8333-333333333333/device/44444444-4444-4444-8444-444444444444"
	if _, err = c.normalized(); err == nil {
		t.Fatal("accepted foreign-tenant source")
	}
}

func TestLoadConfigStrictFileAndJSON(t *testing.T) {
	f, _ := setupPeople(t)
	raw, err := json.Marshal(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(link); err == nil {
		t.Fatal("accepted config symlink")
	}
	if err = os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(path); err == nil {
		t.Fatal("accepted writable commissioning")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"unknown":true}`)...), []byte(`{"version":1,"version":1}`)} {
		if err = os.WriteFile(path, invalid, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadConfig(path); err == nil {
			t.Fatal("accepted non-strict config")
		}
	}
}

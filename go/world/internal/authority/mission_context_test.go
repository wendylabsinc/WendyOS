package authority

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
)

func TestMissionControlRootSignatureScopeAndStrictFields(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/native-mission-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Root    []byte                       `json:"root_public_key"`
		Control missioncontrol.SignedContext `json:"control_context"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if err = VerifyMissionControlContext(fixture.Control, fixture.Root); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*missioncontrol.SignedContext){func(c *missioncontrol.SignedContext) { c.Context.Area.Max[0]++ }, func(c *missioncontrol.SignedContext) { c.Context.RunID = "other" }, func(c *missioncontrol.SignedContext) { c.Context.RequiredSensors[0] = "other" }, func(c *missioncontrol.SignedContext) { c.Context.MaxDurationMS++ }, func(c *missioncontrol.SignedContext) { c.Signature[0] ^= 1 }} {
		copy := missioncontrol.Clone(fixture.Control)
		change(&copy)
		if VerifyMissionControlContext(copy, fixture.Root) == nil {
			t.Fatal("changed policy retained root authority")
		}
	}
	if VerifyMissionControlContext(fixture.Control, ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)) == nil {
		t.Fatal("wrong root accepted")
	}
	raw, _ := json.Marshal(fixture.Control)
	for _, mutation := range []struct{ old, new string }{{`"minimum_energy":0.1`, `"minimum_energy":null`}, {`"version":1,`, ``}, {`"speed_mps":0.25`, `"speed_mps":0.25,"speed_mps":0.5`}, {`"min":[-2,0,-1]`, `"min":[-2,0]`}, {`"min":[-2,0,-1]`, `"min":[-2,0,-1],"min":[-2,0,-1]`}, {`"stop_speed_mps":0.05`, `"stop_speed_mps":null`}} {
		changed := bytes.Replace(raw, []byte(mutation.old), []byte(mutation.new), 1)
		var c missioncontrol.SignedContext
		if json.Unmarshal(changed, &c) == nil {
			t.Fatalf("malformed context normalized: %s", mutation.old)
		}
	}
}

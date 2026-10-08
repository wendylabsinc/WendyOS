package hostedmcp

import "testing"

func TestSelectorPinsExactResourceAndDevice(t *testing.T) {
	org := "11111111-1111-4111-8111-111111111111"
	device := "22222222-2222-4222-8222-222222222222"
	good := "mcp://mcp.wendy.dev/orgs/" + org + "/devices/" + device
	parsed, err := Parse(good)
	if err != nil || parsed.Resource() != "https://mcp.wendy.dev/orgs/"+org+"/mcp" || parsed.Device != device {
		t.Fatal(parsed, err)
	}
	for _, bad := range []string{good + "/", good + "?token=x", good + "#x", "mcp://user@host/orgs/" + org + "/devices/" + device, "http://host/orgs/" + org + "/devices/" + device, "mcp://host/orgs/%31" + org[1:] + "/devices/" + device, "mcp://host/orgs/00000000-0000-0000-0000-000000000000/devices/" + device} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

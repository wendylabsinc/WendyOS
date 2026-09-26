package cloudrelay

import "testing"

func TestBrowserBrokerTarget(t *testing.T) {
	for _, host := range []string{"relay.dev.wendy.sh", "relay.wendy.sh", "eu.relay.wendy.sh", DevelopmentBrowserBroker} {
		for _, input := range []string{host, host + ":443", "https://" + host, "https://" + host + "/", "https://" + host + ":443/"} {
			got, err := BrowserBrokerTarget(input)
			if err != nil || got != host+":443" {
				t.Errorf("%q: got %q, %v", input, got, err)
			}
		}
	}
	for _, host := range []string{"auth.dev.wendy.sh", "api.dev.wendy.sh", "identity.dev.pki.wendy.sh", "unrelated.wendy.sh", "untrusted.relay.wendy.sh"} {
		if _, err := BrowserBrokerTarget("https://" + host); err == nil {
			t.Errorf("accepted non-broker Wendy host %q", host)
		}
	}
	for _, input := range []string{"localhost:443", "127.0.0.1:443", "[::1]:443", "relay.wendy.sh:22", "http://relay.wendy.sh", "relay.wendy.sh.attacker.test:443", "user@relay.wendy.sh:443", "relay.wendy.sh:443/path", "relay.wendy.sh:443?target=localhost", "relay.wendy.sh?", "relay.wendy.sh:443#fragment", "relay.wendy.sh:", "unrelated-nkohwk7hda-uc.a.run.app", "evil.a.run.app", DevelopmentBrowserBroker + ".attacker.test"} {
		if _, err := BrowserBrokerTarget(input); err == nil {
			t.Errorf("accepted forbidden broker %q", input)
		}
	}
}

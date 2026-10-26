package hostnetwork

import (
	"reflect"
	"strings"
	"testing"
)

func TestIngressCleanupPreservesOtherPortsAndForeignMasquerades(t *testing.T) {
	for _, tc := range []struct {
		name, chain, containerIP, lines string
		want                            []string
	}{
		{name: "exact host port", chain: MeshPortsChainName, lines: "-A " + MeshPortsChainName + " -d 127.0.0.1/32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.0.0.2:8080\n" +
			"-A " + MeshPortsChainName + " -d 127.0.0.1/32 -p tcp -m tcp --dport 8080 -j DNAT --to-destination 10.0.0.3:8080\n",
			want: []string{"-t nat -D " + MeshPortsChainName + " -d 127.0.0.1/32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.0.0.2:8080"}},
		{name: "only owned TCP hairpin signature", chain: "POSTROUTING", containerIP: "10.0.0.2", lines: "-A POSTROUTING -s 127.0.0.1/32 -d 10.0.0.2/32 -p tcp -m tcp --dport 80 -j MASQUERADE\n" +
			"-A POSTROUTING -s 127.0.0.1/32 -d 10.0.0.2/32 -p tcp -m tcp --dport 8080 -j MASQUERADE\n" +
			"-A POSTROUTING -s 127.0.0.1/32 -d 10.0.0.3/32 -p tcp -m tcp --dport 80 -j MASQUERADE\n" +
			"-A POSTROUTING -s 127.0.0.1/32 -d 10.0.0.2/32 -p udp -m udp --dport 80 -j MASQUERADE\n" +
			"-A POSTROUTING -s 10.2.0.0/16 -d 10.0.0.2/32 -p tcp -m tcp --dport 80 -j SNAT --to-source 10.0.0.1\n" +
			"-A POSTROUTING -s 127.0.0.1/32 -d 10.0.0.2/32 -o wlan0 -p tcp -m tcp --dport 80 -j MASQUERADE\n" +
			"-A POSTROUTING -s 127.0.0.1/32 -d 10.0.0.2/32 -p tcp -m tcp --dport 8080 -m comment --comment \"leave --dport 80 alone\" -j MASQUERADE\n",
			want: []string{"-t nat -D POSTROUTING -s 127.0.0.1/32 -d 10.0.0.2/32 -p tcp -m tcp --dport 80 -j MASQUERADE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := meshPortsIPTables
			t.Cleanup(func() { meshPortsIPTables = original })
			var deleted []string
			meshPortsIPTables = func(args ...string) ([]byte, error) {
				if len(args) >= 3 && args[2] == "-S" {
					return []byte(tc.lines), nil
				}
				deleted = append(deleted, strings.Join(args, " "))
				return nil, nil
			}
			if err := flushStaleRulesForPort(tc.chain, 80, tc.containerIP, "tcp"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(deleted, tc.want) {
				t.Fatalf("cleanup removed rules outside its ownership: got %q, want %q", deleted, tc.want)
			}
		})
	}
}

//go:build linux

package localmesh

import (
	"bytes"
	"errors"
	"log"
	"net"
	"strings"
	"testing"
)

func TestNormalOwnerClosePreservesFirstControlReason(t *testing.T) {
	_, remote := testDevices(t)
	a, b := net.Pipe()
	defer b.Close()
	link := newStreamNodeLink(a)
	defer link.Close()
	p := &nodePeer{device: remote, id: 7, transport: "lan-tls", link: link,
		control: make(chan ControlMessage, 2)}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	n := &Node{}
	n.closePeer(p, "catalogue-runtime", errors.New("catalogue inventory lifecycle ended"))
	n.closePeer(p, "directory-control", errors.New("later owner cleanup"))
	if got := link.terminalError().Error(); got != "catalogue-runtime: catalogue inventory lifecycle ended" {
		t.Fatalf("first normal owner reason was lost: %s", got)
	}
	row := output.String()
	if strings.Count(row, "local carrier close") != 1 || !strings.Contains(row, "catalogue inventory lifecycle ended") || !strings.Contains(row, "link=7") {
		t.Fatalf("missing single close diagnostic: %s", row)
	}
}

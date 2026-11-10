//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

func TestNormalOwnerClosePreservesFirstControlReason(t *testing.T) {
	_, remote := testDevices(t)
	a, b := net.Pipe()
	defer b.Close()
	link := newStreamNodeLink(a)
	defer link.Close()
	p := &nodePeer{device: remote, id: 7, transport: "lan-tls", link: link,
		control: make(chan ControlMessage, 2)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := asyncdiag.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := asyncdiag.Flush(ctx); err != nil {
			t.Error(err)
		}
		log.SetOutput(previous)
	}()
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

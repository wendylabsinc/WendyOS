package lite

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	wendypb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
)

type handshakeTestLink struct {
	wcomLink
	err     error
	major   uint32
	request *wendypb.WendyComMessage
}

func (l *handshakeTestLink) linkHandshake() error { return nil }

func (l *handshakeTestLink) send(request *wendypb.WendyComMessage) error {
	l.request = request
	return nil
}

func (l *handshakeTestLink) recv(time.Duration) (*wendypb.WendyComMessage, error) {
	if l.err != nil {
		return nil, l.err
	}
	return &wendypb.WendyComMessage{Msg: &wendypb.WendyComMessage_Handshake{
		Handshake: &wendypb.WendyComHandshake{
			HandshakeId: l.request.GetHandshake().GetHandshakeId(),
			Version:     &wendypb.WendyComProtocolVersion{Major: l.major},
		},
	}}, nil
}

func TestHandshakePreservesTransportErrors(t *testing.T) {
	for _, cause := range []error{io.EOF, os.ErrDeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			client := NewClient(fakeEntropy{})
			client.link = &handshakeTestLink{err: cause}
			err := client.handshake()
			if !errors.Is(err, cause) {
				t.Fatalf("handshake error = %v, want wrapped %v", err, cause)
			}
			if strings.Contains(err.Error(), "support") {
				t.Fatalf("transport failure suggests unsupported protocol: %v", err)
			}
			var timeout net.Error
			if errors.Is(cause, os.ErrDeadlineExceeded) && (!errors.As(err, &timeout) || !timeout.Timeout()) {
				t.Fatalf("handshake lost timeout classification: %v", err)
			}
		})
	}
}

func TestHandshakeReportsUnsupportedPeerVersion(t *testing.T) {
	client := NewClient(fakeEntropy{})
	client.link = &handshakeTestLink{major: versionMajor + 1}
	err := client.handshake()
	if err == nil || !strings.Contains(err.Error(), "unsupported device protocol version") {
		t.Fatalf("handshake error = %v, want unsupported protocol version", err)
	}
}

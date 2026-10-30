package sessionbroker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc"
)

type operatorTCPRequest struct {
	Port int `json:"port"`
}

// auxiliaryAddress checks the live device again for every new connection.
// The caller chooses a port on that device, never an arbitrary network address.
// Destination protocols, including registry mTLS, remain end to end.
func (r *operatorTargets) auxiliaryAddress(ctx context.Context, info OperatorTargetInfo, port int) (string, error) {
	target, matched, err := ParseMeshSelector(info.Selector)
	if err != nil || !matched {
		return "", errors.New("invalid mesh TCP target")
	}
	view, err := r.lookupTopology(ctx, target)
	if err != nil {
		return "", err
	}
	adjacent, err := r.reporter()
	if err != nil {
		return "", err
	}
	current, err := operatorManagementTarget(view, adjacent, target, time.Now())
	if err != nil {
		return "", err
	}
	host, _, err := net.SplitHostPort(current.Address)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// DialOperatorTCP owns one bounded stream on the retained operator attachment.
// A browser/local proxy can keep this connection alive independently of the
// setup context; Close releases its broker client and activity lease.
func DialOperatorTCP(ctx context.Context, selector string, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, errors.New("mesh TCP destination port must be between 1 and 65535")
	}
	client, err := operatorControl(ctx, selector)
	if err != nil {
		return nil, err
	}
	conn, err := openOperatorTCP(ctx, client.Conn, selector, port, client.Close)
	if err != nil {
		_ = client.Close()
	}
	return conn, err
}

func openOperatorTCP(ctx context.Context, cc *grpc.ClientConn, selector string, port int, release func() error) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, errors.New("mesh TCP destination port must be between 1 and 65535")
	}
	request, err := json.Marshal(operatorTCPRequest{Port: port})
	if err != nil {
		return nil, err
	}
	return openOperatorStream(ctx, cc, selector, operatorTCPMethod, request, release)
}

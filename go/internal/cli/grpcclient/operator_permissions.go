package grpcclient

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/operatorpermission"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

// RequestOperatorCertificate is installed once by the CLI composition root. It
// uses the existing Cloud OAuth/DPoP refresh and signed-request implementation.
// Library clients may leave it nil and supply entitled credentials themselves.
var RequestOperatorCertificate func(context.Context, *config.CertificateInfo, certs.WendyIdentity) (*config.CertificateInfo, error)

type operatorPermissionDialer struct {
	mu                         sync.Mutex
	expiryTimer                *time.Timer
	login                      *config.CertificateInfo
	target                     certs.WendyIdentity
	dial                       func(context.Context, *config.CertificateInfo) (*AgentConnection, error)
	request                    func(context.Context, *config.CertificateInfo, certs.WendyIdentity) (*config.CertificateInfo, error)
	checked, supported, closed bool
	current                    *AgentConnection
	expires                    time.Time
	retired                    map[*AgentConnection]*time.Timer
}

// connection obtains credentials before sending an operation, never by
// replaying a failed mutation. Reads stay on the original offline connection.
func (d *operatorPermissionDialer) connection(ctx context.Context, base *grpc.ClientConn, permission string) (*AgentConnection, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, fmt.Errorf("operator connection is closed")
	}
	if !d.checked {
		version, err := agentpb.NewWendyAgentServiceClient(base).GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			return nil, err
		}
		d.supported = slices.Contains(version.GetFeatureset(), "operator-permissions-v1")
		d.checked = true
	}
	if !d.supported {
		return nil, nil
	}
	if d.current == nil || !time.Now().Add(30*time.Second).Before(d.expires) {
		cert, err := d.request(ctx, d.login, d.target)
		if err != nil {
			return nil, err
		}
		leaves, err := certs.ParseCertsFromPEM([]byte(cert.PemCertificate))
		if err != nil || len(leaves) != 1 {
			return nil, fmt.Errorf("invalid entitled certificate")
		}
		conn, err := d.dial(ctx, cert)
		if err != nil {
			return nil, err
		}
		if d.expiryTimer != nil {
			d.expiryTimer.Stop()
		}
		if d.current != nil {
			if d.retired == nil {
				d.retired = make(map[*AgentConnection]*time.Timer)
			}
			old := d.current
			d.retired[old] = time.AfterFunc(time.Until(d.expires), func() {
				d.mu.Lock()
				defer d.mu.Unlock()
				if _, ok := d.retired[old]; ok {
					old.Close()
					delete(d.retired, old)
				}
			})
		}
		d.current, d.expires = conn, leaves[0].NotAfter
		d.expiryTimer = time.AfterFunc(time.Until(d.expires), func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.current == conn {
				conn.Close()
				d.current = nil
			}
		})
	}
	// Check the requested permission even during the device's audit phase.
	leaves, err := certs.ParseCertsFromPEM([]byte(d.current.CertInfo.PemCertificate))
	if err != nil || len(leaves) != 1 {
		return nil, fmt.Errorf("invalid entitled certificate")
	}
	policy := operatorpermission.Policy{Scope: d.target.Scope(), DeviceUUID: d.target.EntityID}
	if err := policy.Decide(leaves[0], permission); err != nil {
		return nil, err
	}
	return d.current, nil
}

func (d *operatorPermissionDialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.expiryTimer != nil {
		d.expiryTimer.Stop()
	}
	if d.current != nil {
		d.current.Close()
	}
	for c, timer := range d.retired {
		timer.Stop()
		c.Close()
	}
	d.retired = nil
	return nil
}

func (d *operatorPermissionDialer) unary(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	permission := operatorpermission.RPCPermission(method)
	if permission == "" || permission == "device:read" {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	conn, err := d.connection(ctx, cc, permission)
	if err != nil {
		return err
	}
	if conn == nil {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	return conn.Conn.Invoke(ctx, method, req, reply, opts...)
}

func (d *operatorPermissionDialer) stream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	permission := operatorpermission.RPCPermission(method)
	if permission == "" || permission == "device:read" {
		return streamer(ctx, desc, cc, method, opts...)
	}
	conn, err := d.connection(ctx, cc, permission)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return streamer(ctx, desc, cc, method, opts...)
	}
	return conn.Conn.NewStream(ctx, desc, method, opts...)
}

// CertificateForPermission supplies the same ephemeral credential to adjacent
// protocols such as OCI. It does not replace the persisted login certificate.
func (c *AgentConnection) CertificateForPermission(ctx context.Context, permission string) (*config.CertificateInfo, error) {
	if c.operatorPermissions == nil {
		return c.CertInfo, nil
	}
	conn, err := c.operatorPermissions.connection(ctx, c.Conn, permission)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return c.CertInfo, nil
	}
	return conn.CertInfo, nil
}

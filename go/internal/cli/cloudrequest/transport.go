package cloudrequest

import (
	"context"
	"errors"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/cloud"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// invokeTransport keeps the existing typed grpc-go call boundary, including its
// interceptors and trailer collection. It performs only protobuf conversion.
type invokeTransport struct {
	conn  grpc.ClientConnInterface
	reply proto.Message
}

func (t invokeTransport) Unary(ctx context.Context, r wendy.RPCRequest) (wendy.RPCResponse, error) {
	request := new(pb.SignedRequest)
	if err := proto.Unmarshal(r.Message, request); err != nil {
		return wendy.RPCResponse{}, err
	}
	reply := t.reply
	if r.Method == pb.OperatorSessionService_RegisterOperatorLeaf_FullMethodName {
		reply = new(pb.RegisterOperatorLeafResponse)
	}
	ctx = metadata.NewOutgoingContext(ctx, grpcMetadata(r.Metadata))
	var headers, trailers metadata.MD
	err := t.conn.Invoke(ctx, r.Method, request, reply, grpc.Header(&headers), grpc.Trailer(&trailers))
	response := wendy.RPCResponse{InitialMetadata: coreMetadata(headers), TrailingMetadata: coreMetadata(trailers)}
	if err != nil {
		st := status.Convert(err)
		return response, &displayRPCError{status: &wendy.RPCStatusError{Code: wendy.RPCStatusCode(st.Code()), Message: st.Message(), InitialMetadata: response.InitialMetadata, TrailingMetadata: response.TrailingMetadata, Cause: err}}
	}
	response.Message, err = proto.Marshal(reply)
	return response, err
}
func (t invokeTransport) ServerStream(context.Context, wendy.RPCRequest) (wendy.ServerStream, error) {
	return nil, status.Error(12, "signed Cloud requests are unary")
}

// displayRPCError retains the Core status for retry policy while preserving the
// established CLI diagnostic and grpc-go status/details through Unwrap.
type displayRPCError struct{ status *wendy.RPCStatusError }

func (e *displayRPCError) Error() string {
	if e.status.Cause != nil {
		return e.status.Cause.Error()
	}
	return e.status.Error()
}
func (e *displayRPCError) Unwrap() error { return e.status }

func displayError(err error) error {
	var rpc *wendy.RPCStatusError
	if errors.As(err, &rpc) && rpc.Cause != nil {
		return &displayRPCError{rpc}
	}
	return err
}

func coreMetadata(md metadata.MD) wendy.Metadata {
	var result wendy.Metadata
	for name, values := range md {
		for _, value := range values {
			result = append(result, wendy.MetadataEntry{Name: name, Value: []byte(value)})
		}
	}
	return result
}
func grpcMetadata(md wendy.Metadata) metadata.MD {
	result := metadata.MD{}
	for _, entry := range md {
		result.Append(entry.Name, string(entry.Value))
	}
	return result
}

// Client retains caller-owned connections and per-call credential refresh while
// WendyGo owns Cloud API calls and pagination. The returned client borrows conn.
func Client(conn *grpc.ClientConn, authenticate func(context.Context) (context.Context, error)) (*client, error) {
	transport, err := wendy.NewGRPCTransport(conn)
	if err != nil {
		return nil, err
	}
	return &client{Client: &cloud.Client{Transport: authenticatedTransport{Transport: transport, authenticate: authenticate}}}, nil
}

// client restores host preflight errors after Core's device-list orchestration.
// The CLI historically returned these before opening an RPC, without the
// "listing devices" context used for actual RPC/stream failures.
type client struct{ *cloud.Client }

func (c *client) ListDevices(ctx context.Context, tenant, filter string, onlineOnly bool) ([]*cloud.Asset, error) {
	assets, err := c.Client.ListDevices(ctx, tenant, filter, onlineOnly)
	var preflight *authenticationError
	if errors.As(err, &preflight) {
		return nil, preflight.error
	}
	return assets, err
}

// authenticationError identifies host failures by origin rather than by text,
// preserving the original cause through Core's wrapping and concurrent calls.
type authenticationError struct{ error }

func (e *authenticationError) Unwrap() error { return e.error }

type authenticatedTransport struct {
	wendy.Transport
	authenticate func(context.Context) (context.Context, error)
}

func (t authenticatedTransport) context(ctx context.Context) (context.Context, error) {
	if t.authenticate == nil {
		return ctx, nil
	}
	authenticated, err := t.authenticate(ctx)
	if err != nil {
		return nil, &authenticationError{err}
	}
	return authenticated, nil
}
func (t authenticatedTransport) Unary(ctx context.Context, r wendy.RPCRequest) (wendy.RPCResponse, error) {
	ctx, err := t.context(ctx)
	if err != nil {
		return wendy.RPCResponse{}, err
	}
	response, err := t.Transport.Unary(ctx, r)
	return response, displayError(err)
}
func (t authenticatedTransport) ServerStream(ctx context.Context, r wendy.RPCRequest) (wendy.ServerStream, error) {
	ctx, err := t.context(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := t.Transport.ServerStream(ctx, r)
	if err != nil {
		return nil, displayError(err)
	}
	return displayStream{stream}, nil
}

type displayStream struct{ wendy.ServerStream }

func (s displayStream) Recv() ([]byte, error) {
	data, err := s.ServerStream.Recv()
	return data, displayError(err)
}

// Assets converts SDK wire messages to the CLI's generated types, preserving
// optional/unknown fields and nil entries without reinterpreting product data.
func Assets(records []*cloud.Asset) ([]*pb.Asset, error) {
	result := make([]*pb.Asset, 0, len(records))
	for _, record := range records {
		if record == nil {
			result = append(result, nil)
			continue
		}
		data, err := proto.Marshal(record)
		if err != nil {
			return nil, err
		}
		asset := new(pb.Asset)
		if err := proto.Unmarshal(data, asset); err != nil {
			return nil, err
		}
		result = append(result, asset)
	}
	return result, nil
}

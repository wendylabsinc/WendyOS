package cbridge

/*
#include "objects.h"
*/
import "C"

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud"
	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/internal/bindingpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

var operations = struct {
	sync.Mutex
	next uint64
	live map[uint64]*operation
}{live: make(map[uint64]*operation)}

type operation struct {
	ctx        context.Context
	cancel     context.CancelFunc
	host       uint64
	request    []byte
	completion C.WendyCompletion
	context    unsafe.Pointer
	started    bool // protected by operations.Mutex
}

// RandomBytes obtains secure entropy from the registered host, never Go crypto.
// The byte count is fixed-width big endian so C hosts need no protobuf decoder.
func (h *Host) RandomBytes(ctx context.Context, size int) ([]byte, error) {
	if size <= 0 || size > 1<<20 {
		return nil, fmt.Errorf("invalid host entropy length")
	}
	count := make([]byte, 4)
	binary.BigEndian.PutUint32(count, uint32(size))
	r, err := h.unary(ctx, C.WendyHostRandomBytes, platform.RPCRequest{Message: count})
	if err != nil {
		return nil, err
	}
	if len(r.response.Message) != size {
		return nil, fmt.Errorf("host entropy source returned the wrong number of bytes")
	}
	return r.response.Message, nil
}

var _ platform.Entropy = (*Host)(nil)

//export wendyCloudListDevicesCreate
func wendyCloudListDevicesCreate(host C.WendyHost, request C.WendyDataView, completion C.WendyCompletion, userContext unsafe.Pointer) *C.WendyOperation {
	if completion == nil || request.size > 1<<20 || (request.size != 0 && request.data == nil) {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	o := &operation{ctx: ctx, cancel: cancel, host: uint64(host),
		request: C.GoBytes(unsafe.Pointer(request.data), C.int(request.size)), completion: completion, context: userContext}
	operations.Lock()
	defer operations.Unlock()
	operations.next++
	id := operations.next
	handle := C.operation_new(C.uint64_t(id))
	if handle == nil {
		cancel()
		return nil
	}
	operations.live[id] = o
	return handle
}

//export wendyOperationStart
func wendyOperationStart(id C.uint64_t, handle *C.WendyOperation) {
	operations.Lock()
	defer operations.Unlock()
	o := operations.live[uint64(id)]
	if o == nil || o.started {
		return
	}
	o.started = true
	C.WendyOperationRetain(handle)
	go o.run(handle)
}

//export wendyOperationCancel
func wendyOperationCancel(id C.uint64_t) {
	operations.Lock()
	o := operations.live[uint64(id)]
	operations.Unlock()
	if o != nil {
		o.cancel()
	}
}

//export wendyOperationDispose
func wendyOperationDispose(id C.uint64_t) {
	operations.Lock()
	o := operations.live[uint64(id)]
	delete(operations.live, uint64(id))
	operations.Unlock()
	if o != nil {
		o.cancel()
	}
}

func (o *operation) run(handle *C.WendyOperation) {
	defer C.WendyOperationRelease(handle)
	defer o.cancel()
	response, err := o.listDevices()
	if canceled := o.ctx.Err(); canceled != nil {
		err = canceled
	}
	// Credentials and callback context need not be held by retained operations
	// after delivery. The worker alone reads/writes these fields.
	o.request = nil
	var a arena
	defer a.free()
	if err != nil {
		code, message, details := operationError(err)
		errorObject := C.error_new(C.uint32_t(code), dataView(a.data([]byte(message))), dataView(a.data(details)))
		C.operation_complete(o.completion, o.context, nil, errorObject)
	} else {
		data := C.data_new(dataView(a.data(response)))
		C.operation_complete(o.completion, o.context, data, nil)
	}
	o.completion, o.context = nil, nil
}

func dataView(v C.WendyHostBytes) C.WendyDataView {
	return C.WendyDataView{data: v.data, size: v.size}
}

type invalidInput struct{ message string }

func (e *invalidInput) Error() string { return e.message }

func (o *operation) listDevices() ([]byte, error) {
	var request pb.CloudDeviceListRequest
	if err := proto.Unmarshal(o.request, &request); err != nil {
		return nil, &invalidInput{"invalid CloudDeviceListRequest protobuf"}
	}
	authority := request.GetAuthority()
	endpoint, err := url.Parse("https://" + authority)
	if err != nil || authority == "" || endpoint.Host != authority || endpoint.Hostname() == "" ||
		endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		strings.ContainsAny(authority, "\\?#/ \t\r\n") {
		return nil, &invalidInput{"Cloud authority must be a host with optional port"}
	}
	tenant := request.GetOrganizationId()
	if !canonicalTenant(tenant) {
		return nil, &invalidInput{"Cloud organization must be a canonical tenant UUID"}
	}
	if request.GetAccessToken() == "" {
		return nil, &invalidInput{"Cloud access token is required"}
	}
	host, ok := Lookup(o.host)
	if !ok {
		return nil, &invalidInput{"Cloud host registration is not live"}
	}
	ctx := o.ctx
	if millis := request.GetDeadlineUnixMillis(); millis != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.UnixMilli(millis))
		defer cancel()
	}
	flowID, err := cloud.CorrelationID(ctx, nil, host)
	if err != nil {
		return nil, err
	}
	transport := &cloudTransport{host: host, authority: authority, token: request.GetAccessToken(), flowID: flowID}
	assets, err := (&cloud.Client{Transport: transport}).ListDevices(ctx, tenant, request.GetFilter(), request.GetOnlineOnly())
	if err != nil {
		return nil, err
	}
	response := &pb.CloudDeviceListResponse{Assets: make([][]byte, 0, len(assets))}
	for _, asset := range assets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wire, err := proto.Marshal(asset)
		if err != nil {
			return nil, err
		}
		response.Assets = append(response.Assets, wire)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return proto.Marshal(response)
}

func canonicalTenant(tenant string) bool {
	if len(tenant) != 36 {
		return false
	}
	for i, b := range []byte(tenant) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if b != '-' {
				return false
			}
		} else if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

type cloudTransport struct {
	host                     *Host
	authority, token, flowID string
}

func (t *cloudTransport) authenticate(ctx context.Context, request platform.RPCRequest) (platform.RPCRequest, error) {
	metadata, err := cloud.AuthenticationMetadata(ctx, t.host, t.authority, request.Method, t.token, t.host, request.Metadata)
	request.Metadata = append(metadata, platform.MetadataEntry{Name: "x-correlation-id", Value: []byte(t.flowID)})
	return request, err
}
func (t *cloudTransport) Unary(ctx context.Context, request platform.RPCRequest) (platform.RPCResponse, error) {
	request, err := t.authenticate(ctx, request)
	if err != nil {
		return platform.RPCResponse{}, err
	}
	return t.host.Unary(ctx, request)
}
func (t *cloudTransport) ServerStream(ctx context.Context, request platform.RPCRequest) (platform.ServerStream, error) {
	request, err := t.authenticate(ctx, request)
	if err != nil {
		return nil, err
	}
	return t.host.ServerStream(ctx, request)
}

func operationError(err error) (uint32, string, []byte) {
	code, message := uint32(platform.RPCInternal), err.Error()
	var details pb.RPCErrorDetails
	var status *platform.RPCStatusError
	var input *invalidInput
	switch {
	case errors.As(err, &status):
		code, message = uint32(status.Code), status.Message
		for _, entry := range status.InitialMetadata {
			details.InitialMetadata = append(details.InitialMetadata, &pb.MetadataEntry{Name: entry.Name, Value: entry.Value})
		}
		for _, entry := range status.TrailingMetadata {
			details.TrailingMetadata = append(details.TrailingMetadata, &pb.MetadataEntry{Name: entry.Name, Value: entry.Value})
		}
	case errors.Is(err, context.Canceled):
		code = uint32(platform.RPCCanceled)
	case errors.Is(err, context.DeadlineExceeded):
		code = uint32(platform.RPCDeadlineExceeded)
	case errors.As(err, &input):
		code = uint32(platform.RPCInvalidArgument)
	}
	wire, _ := proto.Marshal(&details) // Byte/string fields; cannot fail.
	return code, message, wire
}

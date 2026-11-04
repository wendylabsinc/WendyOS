package sessionbroker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const operatorListMethod = "/wendy.sessionbroker.DirectoryV1/List"
const operatorWatchMethod = "/wendy.sessionbroker.DirectoryV1/Watch"
const operatorDirectoryBytes = 128 * 1024
const operatorDirectoryRows = 256
const operatorDirectoryWatchers = 16
const operatorDirectoryPoll = time.Second
const operatorDirectoryHeartbeat = 5 * time.Second

// OperatorDirectory projects this broker's locally verified full participant.
// Explicit host fixtures may still inject an authenticated observer snapshot.
type OperatorDirectory struct {
	SourceResponseBytes int                  `json:"sourceResponseBytes,omitempty"`
	PollIntervalMillis  int64                `json:"pollIntervalMillis,omitempty"`
	HeartbeatMillis     int64                `json:"heartbeatMillis,omitempty"`
	Version             int                  `json:"version"`
	State               string               `json:"state"`
	Source              string               `json:"source,omitempty"`
	Captured            int64                `json:"capturedAtUnixMs,omitempty"`
	Truncated           bool                 `json:"truncated,omitempty"`
	Devices             []OperatorTargetInfo `json:"devices"`
}

func (r *operatorTargets) directorySnapshot(ctx context.Context) OperatorDirectory {
	out := OperatorDirectory{Version: 1, State: "unavailable", Devices: []OperatorTargetInfo{}}
	operator, err := operatorPrincipal(r.certificate)
	if err != nil {
		return out
	}
	adjacent, err := r.reporter()
	if err != nil {
		return out
	}
	out.Source = adjacent.Principal()
	view, err := r.topology(ctx)
	if err != nil || view == nil {
		return out
	}
	out.SourceResponseBytes = proto.Size(view)
	out.Captured = view.CapturedAtUnixMs
	out.Truncated = view.Truncated
	now := time.Now()
	if !view.RuntimeAvailable || view.SelfPrincipal != adjacent.Principal() || view.SelfTenantId != operator.Tenant().String() || view.SelfDeviceId != adjacent.UUID().String() || view.SelfMeshAddress != adjacent.MeshAddress().String() || out.Captured > now.Add(time.Second).UnixMilli() || now.UnixMilli()-out.Captured > 10000 {
		return out
	}
	out.State = "ready"
	seen := make(map[string]bool)
	budget := out
	budget.Truncated = true
	budget.PollIntervalMillis = operatorDirectoryPoll.Milliseconds()
	budget.HeartbeatMillis = operatorDirectoryHeartbeat.Milliseconds()
	envelope, _ := json.Marshal(budget)
	bytes := len(envelope) // Include the actual source principal and wrapper.
	for _, hint := range view.Announcements {
		if seen[hint.Principal] {
			continue
		}
		target, err := meshid.DeviceFromPrincipal(hint.Principal)
		if err != nil || target.Tenant() != operator.Tenant() {
			continue
		}
		info, err := operatorManagementTarget(view, adjacent, target, now)
		if err != nil {
			continue
		}
		info.OperatorPrincipal = operator.Principal()
		if r.localSnapshot != nil {
			info.InternetSharing = operatormesh.InternetGateway(r.localSnapshot(), target, now)
		}
		for _, route := range view.Routes {
			if route.Prefix == hint.Prefix && route.OriginPrincipal == info.Principal && route.OriginAuthorized && !route.Unreachable && (!route.Local || target == adjacent) && route.OriginRouterId == hint.OriginRouterId && route.SequenceNumber == hint.SequenceNumber {
				info.Metric = route.Metric
				info.NextHop = route.NextHopPrincipal
				break
			}
		}
		encoded, err := json.Marshal(info)
		if err != nil {
			continue
		}
		if len(out.Devices) >= operatorDirectoryRows || bytes+len(encoded)+1 > operatorDirectoryBytes {
			out.Truncated = true
			break
		}
		bytes += len(encoded) + 1
		seen[info.Principal] = true
		out.Devices = append(out.Devices, info)
	}
	sort.Slice(out.Devices, func(i, j int) bool { return out.Devices[i].Principal < out.Devices[j].Principal })
	return out
}

// directoryPublisher has one polling effect for all bounded watchers. Slow
// readers retain only one replaceable snapshot, never an event history.
type directoryPublisher struct {
	mu               sync.Mutex
	owner            *operatorTargets
	watchers         map[uint64]chan OperatorDirectory
	next, generation uint64
	cancel           context.CancelFunc
	closed           bool
	latest           OperatorDirectory
	hasLatest        bool
}

func (p *directoryPublisher) watch() (chan OperatorDirectory, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, status.Error(codes.Unavailable, "operator attachment is closing")
	}
	if len(p.watchers) >= operatorDirectoryWatchers {
		return nil, nil, status.Error(codes.ResourceExhausted, "operator directory watcher capacity is busy")
	}
	if p.watchers == nil {
		p.watchers = map[uint64]chan OperatorDirectory{}
	}
	p.next++
	id := p.next
	ch := make(chan OperatorDirectory, 1)
	p.watchers[id] = ch
	if p.hasLatest {
		ch <- p.latest
	} else {
		ch <- OperatorDirectory{Version: 1, State: "loading", PollIntervalMillis: operatorDirectoryPoll.Milliseconds(), HeartbeatMillis: operatorDirectoryHeartbeat.Milliseconds(), Devices: []OperatorTargetInfo{}}
	}
	if p.cancel == nil {
		p.generation++
		ctx, cancel := context.WithCancel(context.Background())
		p.cancel = cancel
		go p.run(ctx, p.generation)
	}
	return ch, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if current, ok := p.watchers[id]; ok {
			delete(p.watchers, id)
			close(current)
		}
		if len(p.watchers) == 0 && p.cancel != nil {
			p.cancel()
			p.cancel = nil
			p.generation++
			p.latest = OperatorDirectory{}
			p.hasLatest = false
		}
	}, nil
}
func (p *directoryPublisher) run(ctx context.Context, generation uint64) {
	ticker := time.NewTicker(operatorDirectoryPoll)
	var publication directoryPublication
	defer ticker.Stop()
	for {
		poll, cancel := context.WithTimeout(ctx, 3*time.Second)
		snapshot := p.owner.directorySnapshot(poll)
		cancel()
		snapshot.PollIntervalMillis = operatorDirectoryPoll.Milliseconds()
		snapshot.HeartbeatMillis = operatorDirectoryHeartbeat.Milliseconds()
		emit := publication.shouldEmit(snapshot, time.Now())
		p.mu.Lock()
		if p.closed || generation != p.generation {
			p.mu.Unlock()
			return
		}
		p.latest = snapshot
		p.hasLatest = true
		if emit {
			for _, ch := range p.watchers {
				select {
				case ch <- snapshot:
				default:
					select {
					case <-ch:
					default:
					}
					ch <- snapshot
				}
			}
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (p *directoryPublisher) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.latest = OperatorDirectory{}
	p.hasLatest = false
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	for id, ch := range p.watchers {
		close(ch)
		delete(p.watchers, id)
	}
}

func (r *operatorTargets) localDirectory(stream grpc.ServerStream, a *activity) (bool, error) {
	method, _ := grpc.MethodFromServerStream(stream)
	if method != operatorListMethod && method != operatorWatchMethod {
		return false, nil
	}
	a.activeRPCs.Add(1)
	a.touch()
	defer func() { a.activeRPCs.Add(-1); a.touch() }()
	var request rawMessage
	if err := stream.RecvMsg(&request); err != nil {
		return true, err
	}
	if len(request) != 0 {
		return true, status.Error(codes.InvalidArgument, "directory request has no payload")
	}
	send := func(snapshot OperatorDirectory) error {
		bytes, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if len(bytes) > operatorDirectoryBytes {
			return status.Error(codes.ResourceExhausted, "operator directory snapshot exceeds its bound")
		}
		reply := rawMessage(bytes)
		return stream.SendMsg(&reply)
	}
	if method == operatorListMethod {
		ctx, cancel := context.WithTimeout(stream.Context(), 3*time.Second)
		defer cancel()
		return true, send(r.directorySnapshot(ctx))
	}
	ch, release, err := r.directory.watch()
	if err != nil {
		return true, err
	}
	defer release()
	for {
		select {
		case snapshot, ok := <-ch:
			if !ok {
				return true, nil
			}
			if err = send(snapshot); err != nil {
				return true, err
			}
		case <-stream.Context().Done():
			return true, stream.Context().Err()
		}
	}
}

// connectedOperatorControl never starts a Session, helper or first attachment.
// Nearby can use a currently connected selected profile without making a
// physical/cloud discovery command implicitly join or contact Cloud.
func connectedOperatorControl(ctx context.Context) (*grpcclient.AgentConnection, error) {
	certificate, operator, err := configuredOperator()
	if err != nil {
		return nil, err
	}
	directory, err := configDir()
	if err != nil {
		return nil, err
	}
	recipe, err := readOperatorRecipe(operatorRecipePath(directory, operator.Principal()))
	if err != nil {
		return nil, err
	}
	spec, err := operatorSpec(recipe, certificate)
	if err != nil {
		return nil, err
	}
	return connectOperatorProxy(ctx, spec, &certificate)
}
func ReadOperatorDirectory(ctx context.Context) (OperatorDirectory, error) {
	client, err := connectedOperatorControl(ctx)
	if err != nil {
		return OperatorDirectory{Version: 1, State: "not-connected"}, err
	}
	defer client.Close()
	request, reply := rawMessage{}, rawMessage{}
	err = client.Conn.Invoke(ctx, operatorListMethod, &request, &reply, grpc.ForceCodec(rawCodec{}), grpc.MaxCallRecvMsgSize(operatorDirectoryBytes))
	return decodeOperatorDirectory(reply, err)
}
func decodeOperatorDirectory(bytes rawMessage, err error) (OperatorDirectory, error) {
	if err != nil {
		state := "unavailable"
		if status.Code(err) == codes.Unimplemented {
			state = "unsupported"
		}
		return OperatorDirectory{Version: 1, State: state}, err
	}
	var snapshot OperatorDirectory
	if len(bytes) > operatorDirectoryBytes || json.Unmarshal(bytes, &snapshot) != nil || snapshot.Version != 1 || len(snapshot.Devices) > operatorDirectoryRows {
		return OperatorDirectory{Version: 1, State: "unsupported"}, errors.New("unsupported operator directory IPC")
	}
	return snapshot, nil
}
func WatchOperatorDirectory(ctx context.Context) (<-chan OperatorDirectory, error) {
	client, err := connectedOperatorControl(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := client.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, operatorWatchMethod, grpc.ForceCodec(rawCodec{}), grpc.MaxCallRecvMsgSize(operatorDirectoryBytes))
	if err != nil {
		client.Close()
		return nil, err
	}
	request := rawMessage{}
	if err = stream.SendMsg(&request); err != nil {
		client.Close()
		return nil, err
	}
	stream.CloseSend()
	ch := make(chan OperatorDirectory, 1)
	go func() {
		defer close(ch)
		defer client.Close()
		for {
			var bytes rawMessage
			err := stream.RecvMsg(&bytes)
			if errors.Is(err, io.EOF) {
				return
			}
			snapshot, decode := decodeOperatorDirectory(bytes, err)

			select {
			case ch <- snapshot:
			default:
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- snapshot:
				case <-ctx.Done():
					return
				}
			}
			if decode != nil {
				return
			}
		}
	}()
	return ch, nil
}

// Capture/size are observation bookkeeping, not a device/path change. A
// bounded heartbeat still renews display freshness for unchanged healthy views.
type directoryPublication struct {
	digest      [32]byte
	last        time.Time
	initialized bool
}

func (p *directoryPublication) shouldEmit(snapshot OperatorDirectory, now time.Time) bool {
	snapshot.Captured = 0
	snapshot.SourceResponseBytes = 0
	// The projection contains only fixed scalar/string/slice fields: no custom
	// marshalers, cycles, floats or unsupported JSON values.
	bytes, _ := json.Marshal(snapshot)
	digest := sha256.Sum256(bytes)
	if !p.initialized || digest != p.digest || now.Sub(p.last) >= operatorDirectoryHeartbeat {
		p.initialized = true
		p.digest = digest
		p.last = now
		return true
	}
	return false
}

package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// sseKeepalive is how often an idle event stream gets a comment line, so
// proxies and browsers do not close it.
const sseKeepalive = 15 * time.Second

// serveLiveSSE serves the device's live records to browsers on this computer
// as Server-Sent Events at http://addr/live, until ctx ends. It is the bridge
// for browser viewers until Wendy Cloud relays the stream: Companion web reads
// it with VITE_WENDY_DATA_LIVE_URL=http://addr/live.
//
// Each request opens its own StreamData subscription with req's selection,
// unless its query names streams. The proxy has no authentication of its own,
// so it listens only on a loopback address, answers only requests addressed
// to one, and allows only loopback origins.
func serveLiveSSE(ctx context.Context, client agentpbv2.DataServiceClient, addr string, req *agentpbv2.StreamDataRequest, errOut io.Writer) error {
	if err := requireLoopbackAddr(addr); err != nil {
		return err
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           liveSSEHandler(client, req),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	fmt.Fprintf(errOut, "Serving live records at http://%s/live. Press Ctrl-C to stop.\n", lis.Addr())
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err = server.Serve(lis); errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func requireLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" || !isLoopbackHost(host) {
		return fmt.Errorf("--sse needs a loopback address such as 127.0.0.1:8090, not %q: the proxy serves device data without authentication", addr)
	}
	return nil
}

func liveSSEHandler(client agentpbv2.DataServiceClient, base *agentpbv2.StreamDataRequest) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
		// A page from another site may rebind its own name to 127.0.0.1, or
		// fetch this address directly. Neither may read device data.
		if !isLoopbackHost(r.Host) {
			http.Error(w, "the live proxy answers only requests to a loopback address", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !isLoopbackHost(u.Host) {
				http.Error(w, "the live proxy allows only pages served from this computer", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		serveLiveEvents(r.Context(), w, flusher, client, liveSSERequest(base, r.URL.Query()))
	})
	return mux
}

// liveSSERequest is base, with its selection replaced when the query names
// streams: repeated stream, prefix and schema parameters, as wendy-data-app
// takes ?stream=.
func liveSSERequest(base *agentpbv2.StreamDataRequest, q url.Values) *agentpbv2.StreamDataRequest {
	req := proto.Clone(base).(*agentpbv2.StreamDataRequest)
	if q.Has("stream") || q.Has("prefix") || q.Has("schema") {
		req.StreamIds, req.StreamPrefixes, req.Schemas = q["stream"], q["prefix"], q["schema"]
	}
	return req
}

func serveLiveEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, client agentpbv2.DataServiceClient, req *agentpbv2.StreamDataRequest) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.StreamData(ctx, req)
	if err == nil {
		if md, _ := stream.Header(); md == nil {
			_, err = stream.Recv()
		}
	}
	if err != nil {
		if err = liveDataError(ctx, err); err != nil {
			http.Error(w, err.Error(), httpStatusOf(err))
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Receive on its own goroutine, one record at a time: a slow browser
	// holds the next Recv, and the agent keeps only the newest record of
	// each stream meanwhile.
	type received struct {
		record *agentpbv2.DataRecord
		err    error
	}
	records := make(chan received)
	go func() {
		for {
			r, err := stream.Recv()
			select {
			case records <- received{r, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
		case got := <-records:
			if got.err != nil {
				return
			}
			b, err := json.Marshal(liveJSON(got.record))
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
		}
		flusher.Flush()
	}
}

func httpStatusOf(err error) int {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.PermissionDenied, codes.Unauthenticated:
		return http.StatusForbidden
	case codes.ResourceExhausted:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// liveJSONRecord is a DataRecord as browsers receive it, in wendy-data-app's
// shape: protobuf JSON field names, the timestamp as a string as protobuf JSON
// writes an int64, and the payload as JSON instead of bytes.
type liveJSONRecord struct {
	StreamID           string          `json:"stream_id"`
	TimestampUnixNanos int64           `json:"timestamp_unix_nanos,string"`
	Schema             string          `json:"schema"`
	Payload            json.RawMessage `json:"payload"`
	Skipped            uint64          `json:"skipped,omitempty,string"`
}

func liveJSON(r *agentpbv2.DataRecord) liveJSONRecord {
	return liveJSONRecord{
		StreamID:           r.GetStreamId(),
		TimestampUnixNanos: r.GetTimestampUnixNanos(),
		Schema:             r.GetSchema(),
		Payload:            livePayloadJSON(r.GetSchema(), r.GetPayload()),
		Skipped:            r.GetSkipped(),
	}
}

// livePayloadJSON renders a payload for a browser. A schema this CLI has
// compiled in becomes protobuf JSON with proto field names. A payload that is
// already a JSON object or array, as an app stream of media type
// application/json carries, is passed through. Anything else is a base64
// string, as protobuf JSON writes bytes.
func livePayloadJSON(schema string, payload []byte) json.RawMessage {
	var out bytes.Buffer
	if t, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(schema)); err == nil {
		m := t.New().Interface()
		if proto.Unmarshal(payload, m) == nil {
			if b, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(m); err == nil && json.Compact(&out, b) == nil {
				return out.Bytes()
			}
		}
	}
	if trimmed := bytes.TrimSpace(payload); len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Compact(&out, trimmed) == nil {
		return out.Bytes()
	}
	b, _ := json.Marshal(base64.StdEncoding.EncodeToString(payload))
	return b
}

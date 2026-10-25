package commands

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var updateLiveSSEFixture = flag.Bool("update-live-sse", false, "rewrite testdata/live-sse.txt")

func TestLivePayloadJSON(t *testing.T) {
	catalog := catalogDataRecord(t, &agentpbv2.DataStreamInfo{StreamId: "apps/a/s", State: agentpbv2.DataStreamInfo_STATE_ACTIVE, RateHz: 2})
	for _, tc := range []struct {
		name    string
		schema  string
		payload []byte
		want    string
	}{
		{"compiled-in schema", catalog.GetSchema(), catalog.GetPayload(), `{"stream_id":"apps/a/s","state":"STATE_ACTIVE","rate_hz":2}`},
		{"JSON object", "wendy.data.v1.VideoMarkers", []byte(" {\n \"camera\": \"/dev/video0\" }\n"), `{"camera":"/dev/video0"}`},
		{"JSON array", "", []byte(`[1, 2]`), `[1,2]`},
		{"binary", "test.v1.Unknown", []byte{1, 2}, `"AQI="`},
		{"not quite JSON", "", []byte(`{"a":`), `"eyJhIjo="`},
		{"undecodable compiled-in schema", catalog.GetSchema(), []byte{0xff}, `"/w=="`},
		{"empty", "", nil, `""`},
	} {
		if got := string(livePayloadJSON(tc.schema, tc.payload)); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The sample a record describes travels with it, 64-bit numbers as strings as
// protobuf JSON writes them, and is left out when the record names none.
func TestLiveJSONSample(t *testing.T) {
	for _, tc := range []struct {
		record *agentpbv2.DataRecord
		want   string
	}{
		{&agentpbv2.DataRecord{StreamId: "s", TimestampUnixNanos: 1, SourceId: "v4l2:/dev/video0", SampleId: 42},
			`{"stream_id":"s","timestamp_unix_nanos":"1","schema":"","payload":"","source_id":"v4l2:/dev/video0","sample_id":"42"}`},
		{&agentpbv2.DataRecord{StreamId: "s", TimestampUnixNanos: 1},
			`{"stream_id":"s","timestamp_unix_nanos":"1","schema":"","payload":""}`},
	} {
		got, err := json.Marshal(liveJSON(tc.record))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("got %s, want %s", got, tc.want)
		}
	}
}

func TestRequireLoopbackAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8090", "localhost:8090", "[::1]:8090", "127.0.0.1:0"} {
		if err := requireLoopbackAddr(addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{":8090", "0.0.0.0:8090", "192.168.1.5:8090", "[::]:8090", "example.com:8090", "127.0.0.1"} {
		if err := requireLoopbackAddr(addr); err == nil {
			t.Errorf("%s: accepted", addr)
		}
	}
}

// liveSSEFixtureRecords are the records behind testdata/live-sse.txt. That file
// is what a browser reads from the proxy; Companion web's readLiveStream
// parses the markers record in it. The markers payload is protobuf JSON, as an
// app stream of media type application/json carries it; a protobuf payload
// renders the same once this CLI compiles the schema in.
func liveSSEFixtureRecords(t *testing.T) []*agentpbv2.DataRecord {
	return []*agentpbv2.DataRecord{
		catalogDataRecord(t, &agentpbv2.DataStreamInfo{StreamId: "apps/sh.wendy.demo/detector/markers", Schema: "wendy.data.v1.VideoMarkers", MediaType: "application/json", Publisher: "sh.wendy.demo", State: agentpbv2.DataStreamInfo_STATE_ACTIVE, RateHz: 10, LastTimestampUnixNanos: 1792936987250000000}),
		{StreamId: "apps/sh.wendy.demo/detector/markers", TimestampUnixNanos: 1792936987250000000, Schema: "wendy.data.v1.VideoMarkers", Skipped: 3,
			Payload: []byte(`{"camera":"/dev/video0","valid_for_nanos":"500000000","markers":[{"x":0.25,"y":0.5,"width":0.1,"height":0.2,"label":"person","value":0.91,"color":"green"}]}`)},
		{StreamId: "apps/sh.wendy.demo/detector/raw", TimestampUnixNanos: 1792936987260000000, Payload: []byte{0, 1, 2}},
	}
}

func getLive(t *testing.T, server *httptest.Server, path string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if host := header.Get("Host"); host != "" {
		req.Host = host
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestLiveSSEServesRecords(t *testing.T) {
	client := &liveDataClient{stream: &liveDataClientStream{header: metadata.MD{}, records: liveSSEFixtureRecords(t)}}
	base := &agentpbv2.StreamDataRequest{StreamIds: []string{"apps/x/s"}, MaxRateHz: 5, SendLatest: true}
	server := httptest.NewServer(liveSSEHandler(client, base))
	defer server.Close()
	resp, body := getLive(t, server, "/live", http.Header{"Origin": {"http://localhost:5173"}, "Accept": {"text/event-stream"}})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatalf("response %d %v", resp.StatusCode, resp.Header)
	}
	if got := client.request; len(got.GetStreamIds()) != 1 || got.GetMaxRateHz() != 5 || !got.GetSendLatest() {
		t.Fatalf("request: %v", got)
	}
	fixture := filepath.Join("testdata", "live-sse.txt")
	if *updateLiveSSEFixture {
		if err := os.WriteFile(fixture, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if body != string(want) {
		t.Fatalf("body:\n%s\nwant:\n%s", body, want)
	}
	// Each event is one JSON record on one data line.
	for _, event := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		var r map[string]any
		if !strings.HasPrefix(event, "data: ") || strings.Contains(event, "\n") || json.Unmarshal([]byte(strings.TrimPrefix(event, "data: ")), &r) != nil {
			t.Fatalf("event %q", event)
		}
	}

	// Query parameters replace the selection, and keep the rest.
	client = &liveDataClient{stream: &liveDataClientStream{header: metadata.MD{}}}
	server2 := httptest.NewServer(liveSSEHandler(client, base))
	defer server2.Close()
	if resp, _ = getLive(t, server2, "/live?prefix=_streams/&schema=s1&schema=s2", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("query: %d", resp.StatusCode)
	}
	if got := client.request; len(got.GetStreamIds()) != 0 || strings.Join(got.GetStreamPrefixes(), ",") != "_streams/" || strings.Join(got.GetSchemas(), ",") != "s1,s2" || got.GetMaxRateHz() != 5 {
		t.Fatalf("query request: %v", got)
	}
	if len(base.GetStreamPrefixes()) != 0 {
		t.Fatal("a query changed the command line's request")
	}
}

func TestLiveSSERefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		end    error
		code   int
		body   string
	}{
		{"page from another site", http.Header{"Origin": {"https://example.com"}}, nil, http.StatusForbidden, "pages served from this computer"},
		{"rebound host name", http.Header{"Host": {"evil.example:8090"}}, nil, http.StatusForbidden, "loopback address"},
		{"refused by the agent", nil, status.Error(codes.PermissionDenied, "streaming live data requires a client certificate"), http.StatusForbidden, "client certificate"},
		{"bad selection", nil, status.Error(codes.InvalidArgument, "max_rate_hz must be"), http.StatusBadRequest, "max_rate_hz"},
		{"older agent", nil, status.Error(codes.Unimplemented, "unknown method"), http.StatusBadGateway, "update the agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &liveDataClient{stream: &liveDataClientStream{end: tc.end}}
			server := httptest.NewServer(liveSSEHandler(client, &agentpbv2.StreamDataRequest{}))
			defer server.Close()
			resp, body := getLive(t, server, "/live", tc.header)
			if resp.StatusCode != tc.code || !strings.Contains(body, tc.body) {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, body, tc.code, tc.body)
			}
		})
	}
}

// A request that cannot use the events opens no device subscription: a HEAD,
// and a cross-site request without an Origin, such as an <img> or a no-cors
// fetch, which cannot read the response. A loopback page's cross-site
// EventSource sends its Origin and is served.
func TestLiveSSEOpensNoUnusableSubscription(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		header http.Header
		code   int
	}{
		{"HEAD", http.MethodHead, nil, http.StatusMethodNotAllowed},
		{"cross-site without an Origin", http.MethodGet, http.Header{"Sec-Fetch-Site": {"cross-site"}}, http.StatusForbidden},
		{"loopback page on another port", http.MethodGet, http.Header{"Origin": {"http://localhost:5173"}, "Sec-Fetch-Site": {"cross-site"}}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &liveDataClient{stream: &liveDataClientStream{header: metadata.MD{}}}
			server := httptest.NewServer(liveSSEHandler(client, &agentpbv2.StreamDataRequest{}))
			defer server.Close()
			req, err := http.NewRequest(tc.method, server.URL+"/live", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = tc.header.Clone()
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if subscribed := client.request != nil; resp.StatusCode != tc.code || subscribed != (tc.code == http.StatusOK) {
				t.Fatalf("got %d, subscribed %v; want %d", resp.StatusCode, subscribed, tc.code)
			}
		})
	}
}

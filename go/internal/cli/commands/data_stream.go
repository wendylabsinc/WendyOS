package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newDataStreamCmd() *cobra.Command {
	var (
		req agentpbv2.StreamDataRequest
		sse string
	)
	c := &cobra.Command{
		Use:   "stream",
		Short: "Print live records from the device's data streams",
		Long: `Print records from the device's data streams as the agent accepts them,
until interrupted. Every app recording stream is included, with no change to
the app. Stream ids are apps/<app-id>/<service>/<stream>, or
apps/<app-id>/<stream> for an app without a service name.

--stream and --prefix choose streams; --schema keeps only the chosen streams
with that schema. The catalog of streams is stream _streams/<stream-id>; it is
printed only when you select it, for example with --prefix _streams/. "wendy
data streams" lists it as a table.

Delivery is best effort: when the connection is slower than a stream, newer
records replace older ones and only the newest of each stream is printed.
Each stream is also capped at --rate records per second, the device's 30 by
default; a newer record replaces one the cap holds back. --latest starts with
the newest record of each stream, if it is recent.

With --json each record is one line of protobuf JSON, payload in base64.

With --sse ADDR the records are not printed: they are served at
http://ADDR/live as Server-Sent Events, for a browser on this computer. Each
event is one JSON record with the payload as protobuf JSON when this CLI knows
its schema. Query parameters stream, prefix and schema replace the command
line's selection for that request.`,
		Example: `  wendy data stream
  wendy data stream --stream apps/sh.wendy.demo/detector/markers
  wendy data stream --prefix apps/sh.wendy.demo/ --schema wendy.data.v1.VideoMarkers --latest
  wendy data stream --rate 5 --json
  wendy data stream --sse 127.0.0.1:8090`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sse == "" {
				return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
					return streamLiveData(cmd.Context(), client, &req, cmd.OutOrStdout(), cmd.ErrOrStderr(), jsonOutput)
				})
			}
			// Bind first, so a bad or busy address fails before the device
			// connection does any work.
			lis, err := listenLiveSSE(sse)
			if err != nil {
				return err
			}
			defer lis.Close()
			return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
				return serveLiveSSE(cmd.Context(), client, lis, &req, cmd.ErrOrStderr())
			})
		},
	}
	c.Flags().StringArrayVar(&req.StreamIds, "stream", nil, "Only this stream id (repeatable)")
	c.Flags().StringArrayVar(&req.StreamPrefixes, "prefix", nil, "Only stream ids that start with this prefix (repeatable)")
	c.Flags().StringArrayVar(&req.Schemas, "schema", nil, "Only streams with this payload schema (repeatable)")
	c.Flags().Float32Var(&req.MaxRateHz, "rate", 0, "At most this many records per second of each stream, 0.01 to 1000 (default: the device's, 30)")
	c.Flags().BoolVar(&req.SendLatest, "latest", false, "Start with the newest record of each stream, if it is at most 10 seconds old")
	c.Flags().StringVar(&sse, "sse", "", "Serve the records to browsers as Server-Sent Events at this loopback address, for example 127.0.0.1:8090")
	return c
}

// streamLiveData prints records until the stream ends or ctx is cancelled,
// which is how Ctrl-C ends it, and is not an error.
func streamLiveData(ctx context.Context, client agentpbv2.DataServiceClient, req *agentpbv2.StreamDataRequest, out, errOut io.Writer, asJSON bool) error {
	stream, err := client.StreamData(ctx, req)
	if err != nil {
		return liveDataError(ctx, err)
	}
	// Headers arrive once the agent has subscribed this stream; a refused
	// stream has none, and Recv below returns its error.
	if md, _ := stream.Header(); md != nil && !asJSON {
		fmt.Fprintln(errOut, "Waiting for live records. Press Ctrl-C to stop.")
	}
	for {
		r, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return liveDataError(ctx, err)
		}
		if asJSON {
			err = encodeProtoJSON(out, r)
		} else {
			err = writeLiveRecord(out, r)
		}
		if err != nil {
			return err
		}
	}
}

func liveDataError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	if status.Code(err) == codes.Unimplemented {
		return errors.New("this agent does not serve live data streams; update the agent on the target device")
	}
	return err
}

// maxLivePreview bounds the text preview so one record stays one line.
const maxLivePreview = 80

// writeLiveRecord prints one record as a line: the time the data describes,
// the stream id, the schema, the payload size, how many records of the stream
// were skipped before it and, for a text payload, the start of the text.
func writeLiveRecord(out io.Writer, r *agentpbv2.DataRecord) error {
	schema := r.GetSchema()
	if schema == "" {
		schema = "-"
	}
	line := fmt.Sprintf("%s  %s  %s  %d B", time.Unix(0, r.GetTimestampUnixNanos()).Format("15:04:05.000"), r.GetStreamId(), schema, len(r.GetPayload()))
	if n := r.GetSkipped(); n > 0 {
		line += fmt.Sprintf(" (%d skipped)", n)
	}
	if preview, ok := textPreview(r.GetPayload()); ok {
		line += "  " + preview
	}
	_, err := fmt.Fprintln(out, line)
	return err
}

// textPreview returns printable UTF-8 on one line, or false for binary data.
func textPreview(payload []byte) (string, bool) {
	if len(payload) == 0 || !utf8.Valid(payload) {
		return "", false
	}
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(string(payload)) {
		if unicode.IsSpace(r) {
			r = ' '
		} else if !unicode.IsPrint(r) {
			return "", false
		}
		if n == maxLivePreview {
			b.WriteString("...")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String(), b.Len() > 0
}

package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
)

func TestQuerySelectedLinuxAvoidsUnrequestedSchemaWorkAndKeepsTombstones(t *testing.T) {
	sink, err := rpctrace.New(rpctrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	f := setupModuleWithDependencies(t, func(d *Dependencies) { d.Trace = sink }, accessConfig)
	a := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	b := accessSource("telemetry-b", "wendy.sensor.scalar.v1", "gas-b")
	writer := moduleDial(t, f.mount(t, 0, 11).SocketPath)
	other := moduleDial(t, f.mount(t, 1, 12).SocketPath)
	reader := moduleDial(t, accessMount(t, f, a, b).SocketPath)
	original := modulePublish(t, writer, moduleRequest("gas"))
	q := moduleRequest("gas")
	q.Stream = b.Stream
	modulePublish(t, other, q)
	all := accessQuery(t, reader, []serviceaccess.Source{a, b}, 2, "")
	if len(all.SignedRecordJson) != 2 {
		t.Fatal("complete query lost an observation")
	}
	for _, explicit := range []bool{false, true} {
		selected := a
		if explicit {
			selected.Device = f.config.Policy.Device
		}
		out := accessQuery(t, reader, []serviceaccess.Source{selected}, 1, "")
		if len(out.SignedRecordJson) != 1 || !bytes.Equal(out.SignedRecordJson[0], original.SignedRecordJson) {
			t.Fatal("selected query changed original bytes")
		}
	}
	q.Tombstone = true
	q.PayloadJson = nil
	modulePublish(t, other, q)
	out := accessQuery(t, reader, []serviceaccess.Source{a}, 1, "")
	if len(out.SignedRecordJson) != 1 || !bytes.Equal(out.SignedRecordJson[0], original.SignedRecordJson) {
		t.Fatal("unrelated tombstone affected selection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if metadata, err := reader.GetSourceAuthority(ctx, sourceAuthorityRequest(b)); err != nil || metadata == nil {
		t.Fatal("tombstone incorrectly revoked actor", err)
	}
	if dead := accessQuery(t, reader, []serviceaccess.Source{b}, 1, ""); len(dead.SignedRecordJson) != 0 {
		t.Fatal("actor authority revived tombstoned observation")
	}
	f.manager.Close()
	sink.Close()
	var log bytes.Buffer
	if err := sink.Run(context.Background(), &log); err != nil {
		t.Fatal(err)
	}
	var counts []uint64
	for _, line := range bytes.Split(bytes.TrimSpace(log.Bytes()), []byte{'\n'}) {
		var event rpctrace.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Method != "Query" || event.Service != "reader" {
			continue
		}
		if event.Code != "OK" {
			t.Fatal("query was refused", event.Code)
		}
		var count uint64
		for _, stage := range event.Stages {
			if stage.Stage == "journal_schema" {
				count = stage.Count
			}
		}
		counts = append(counts, count)
	}
	want := []uint64{4, 2, 2, 2, 0}
	if len(counts) != len(want) {
		t.Fatal("missing query traces", counts)
	}
	for i := range want {
		if counts[i] != want[i] {
			t.Fatal("query validated unrequested data or omitted a selected validation pass", counts)
		}
	}
}

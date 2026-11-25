//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/qdl"
)

// The actual Firehose session streams payloads and parses these device replies.
// Replies are armed by writes so draining one response cannot consume the next.
type dragonwingTraceConn struct {
	replies [][]byte
	armed   bool
	reads   int
	writes  int
	commit  func(int)
}

func (c *dragonwingTraceConn) Read(p []byte, _ time.Duration) (int, error) {
	if !c.armed || len(c.replies) == 0 {
		return 0, qdl.ErrTimeout
	}
	if c.reads%2 == 1 {
		c.commit(c.reads / 2)
	}
	c.reads++
	c.armed = false
	response := c.replies[0]
	c.replies = c.replies[1:]
	return copy(p, response), nil
}

func (c *dragonwingTraceConn) Write(p []byte, _ time.Duration) (int, error) {
	c.writes++
	if !bytes.HasPrefix(p, []byte("<?xml")) {
		// Real progress is throttled to 66 ms; replay data chunks more slowly.
		time.Sleep(70 * time.Millisecond)
	}
	c.armed = true
	return len(p), nil
}

func (*dragonwingTraceConn) Close() error { return nil }

func TestDragonwingProgressTraceWaitsForPartitionAcknowledgement(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "commit refused"}[refused], func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "payload.img"), bytes.Repeat([]byte{0x42}, 2<<20), 0o600); err != nil {
				t.Fatal(err)
			}
			plan := &qdl.FlashPlan{Dir: dir}
			for _, label := range []string{"rootfs_a", "config"} {
				plan.Programs = append(plan.Programs, qdl.ProgramEntry{Filename: "payload.img", Label: label,
					SectorSize: 512, NumSectors: 4096, StartSector: "2048"})
			}
			ack := []byte(`<?xml version="1.0"?><data><response value="ACK"/></data>`)
			conn := &dragonwingTraceConn{replies: [][]byte{ack, ack, ack, ack}}
			if refused {
				conn.replies[1] = []byte(`<?xml version="1.0"?><data><response value="NAK"/></data>`)
			}
			var out bytes.Buffer
			conn.commit = func(index int) {
				if strings.Contains(out.String(), `wrote "`+plan.Programs[index].Label+`"`) {
					t.Fatal("reported completion before the device acknowledged its partition")
				}
			}
			var trace []string
			err := programDragonwingPartitions(context.Background(), plan, qdl.NewSession(conn, nil), &out,
				func(message string) { trace = append(trace, message) })
			want := []string{`"rootfs_a" · 50% of partition`, `"rootfs_a" · 99% of partition`}
			if refused {
				if err == nil || out.Len() != 0 || conn.writes != 3 {
					t.Fatalf("refused commit: error=%v output=%q writes=%d", err, out.String(), conn.writes)
				}
			} else {
				want = append(want, `"config" · 50% of partition`, `"config" · 99% of partition`, "updating partition table")
				if err != nil || out.String() != "wrote \"rootfs_a\"\nwrote \"config\"\n" {
					t.Fatalf("committed partitions: error=%v output=%q", err, out.String())
				}
			}
			if !reflect.DeepEqual(trace, want) {
				t.Fatalf("progress trace=%q, want %q", trace, want)
			}
		})
	}
}

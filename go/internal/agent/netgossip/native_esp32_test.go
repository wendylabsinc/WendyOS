//go:build esp32lab

package netgossip

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The native fixture links the same C scheduler shipped in the IDF component.
// All messages below cross the real Go implementation; no protocol reimplementation
// in the test is used to decide transfer/digest/ownership behavior.
func TestNativeESP32AnnouncementInterop(t *testing.T) {
	path := os.Getenv("WENDY_NATIVE_GOSSIP_ORACLE")
	if path == "" {
		t.Skip("set WENDY_NATIVE_GOSSIP_ORACLE to the built C fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	call := func(command string) []string {
		t.Helper()
		if _, err := fmt.Fprintln(in, command); err != nil {
			t.Fatal(err)
		}
		var lines []string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "OK ") {
				if line != "OK 0" {
					t.Fatalf("native command failed: %s: %s", command, line)
				}
				return lines
			}
			lines = append(lines, line)
		}
		t.Fatalf("native fixture ended: %v", scanner.Err())
		return nil
	}
	decode := func(line string) Message {
		t.Helper()
		f := strings.Fields(line)
		if len(f) != 8 || f[0] != "M" {
			t.Fatal("invalid native output", line)
		}
		integer := func(s string) uint64 {
			n, e := strconv.ParseUint(s, 10, 64)
			if e != nil {
				t.Fatal(e)
			}
			return n
		}
		op := []string{"have", "want", "unavailable", "data"}
		index := integer(f[1])
		if index >= uint64(len(op)) {
			t.Fatal("invalid operation")
		}
		digest, e := hex.DecodeString(f[3])
		if e != nil {
			t.Fatal(e)
		}
		var data []byte
		if f[7] != "-" {
			data, e = hex.DecodeString(f[7])
			if e != nil {
				t.Fatal(e)
			}
		}
		m := Message{Version: Version, Type: uint8(integer(f[2])), Op: op[index], Digest: digest, Request: integer(f[4]), Offset: int(integer(f[5])), Total: int(integer(f[6])), Data: data}
		if e = m.Validate(); e != nil {
			t.Fatal(e)
		}
		return m
	}
	encode := func(m Message, now int) string {
		op := map[string]int{"have": 0, "want": 1, "unavailable": 2, "data": 3}
		data := "-"
		if len(m.Data) > 0 {
			data = hex.EncodeToString(m.Data)
		}
		return fmt.Sprintf("recv %d %x %d %d %d %d %s", op[m.Op], m.Digest, m.Request, m.Offset, m.Total, now, data)
	}
	now := time.Unix(1700000000, 0)
	pool := NewPool()
	session := pool.NewSession(Manifest)
	defer session.Close()
	payload := bytes.Repeat([]byte("signed manifest envelope\x00"), 215)
	have := session.Offer(payload, now.Add(time.Minute), now)
	lines := call(encode(*have, 100))
	if len(lines) != 1 {
		t.Fatal("expected native WANT", lines)
	}
	want := decode(lines[0])
	first, _, err := session.Receive(want, now)
	if err != nil {
		t.Fatal(err)
	}
	var completed []byte
	deliver := func(m *Message) {
		if m == nil {
			return
		}
		for _, line := range call(encode(*m, 101)) {
			if strings.HasPrefix(line, "C ") {
				var e error
				completed, e = hex.DecodeString(strings.TrimPrefix(line, "C "))
				if e != nil {
					t.Fatal(e)
				}
			} else {
				t.Fatal("unexpected response", line)
			}
		}
	}
	deliver(first)
	for rounds := 0; len(completed) == 0 && rounds < 100; rounds++ {
		for _, m := range session.Poll(now, 4) {
			deliver(m)
		}
	}
	if !bytes.Equal(completed, payload) {
		t.Fatal("Go to native transfer differs")
	}
	// Native HAVE for the same verified object produces no new WANT.
	if got := call(encode(*have, 102)); len(got) != 0 {
		t.Fatal("duplicate download", got)
	}
	// Reverse direction uses a different object, including several full chunks.
	reverse := bytes.Repeat([]byte("native route authorization\x00"), 177)
	lines = call("offer " + hex.EncodeToString(reverse))
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "I ") {
		t.Fatal(lines)
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(lines[0], "I "))
	if err != nil {
		t.Fatal(err)
	}
	expected := Identify(Manifest, reverse)
	if !bytes.Equal(digest, expected.Hash[:]) {
		t.Fatal("Go/C artifact digests differ")
	}
	wantReply, _, err := session.Receive(Message{Version: Version, Op: "have", Type: Manifest, Digest: digest}, now)
	if err != nil || wantReply == nil {
		t.Fatal("Go did not request native object", err)
	}
	call(encode(*wantReply, 103))
	completed = nil
	for round := 0; len(completed) == 0 && round < 100; round++ {
		for _, line := range call("poll 104") {
			m := decode(line)
			reply, data, e := session.Receive(m, now)
			if e != nil {
				t.Fatal(e)
			}
			if reply != nil {
				call(encode(*reply, 104))
			}
			if data != nil {
				completed = data
			}
		}
	}
	if !bytes.Equal(completed, reverse) {
		t.Fatal("native to Go transfer differs")
	}
	session.Resolve(completed, now.Add(time.Minute), now, true)
	t.Logf("Go/C/Go digest and bidirectional chunk transfers pass: %d/%d bytes", len(payload), len(reverse))
}

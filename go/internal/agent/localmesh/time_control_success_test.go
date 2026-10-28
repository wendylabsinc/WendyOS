package localmesh

import (
	"bytes"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"io"
	"os"
	"os/exec"
	"testing"
)

func TestNormalTimestampControlNativeInterop(t *testing.T) {
	oracle := os.Getenv("WENDY_TIME_CONTROL_ORACLE")
	if oracle == "" {
		t.Skip("set WENDY_TIME_CONTROL_ORACLE to native success fixture")
	}
	data, err := exec.Command(oracle, "--wire").CombinedOutput()
	if err != nil {
		t.Fatal(err, string(data))
	}
	stream := bytes.NewReader(data)
	var received []byte
	requests := 0
	for {
		message, readErr := ReadControl(stream)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		var frame bytes.Buffer
		if err = WriteControl(&frame, message); err != nil {
			t.Fatal(err)
		}
		message, err = ReadControl(&frame)
		if err != nil {
			t.Fatal(err)
		}
		x := message.Time
		if message.Kind != "time" || x.ID != 42 {
			t.Fatal("native timestamp identity changed")
		}
		switch x.Op {
		case "request":
			requests++
			if len(x.Data) != 93 {
				t.Fatal("request size changed")
			}
		case "next":
			if int(x.Offset) != len(received) {
				t.Fatal("pull offset changed")
			}
		case "reply":
			if int(x.Offset) != len(received) || x.Total != 17449 {
				t.Fatal("reply boundaries changed")
			}
			received = append(received, x.Data...)
			// The first offset must be explicit for the constrained C reader.
			wire, err := netwire.Marshal(message.Time)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]netwire.RawMessage
			if err = netwire.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			if _, ok := fields[netwire.Key("offset")]; !ok {
				t.Fatal("initial offset omitted")
			}
		default:
			t.Fatal("unexpected normal timestamp operation", x.Op)
		}
	}
	if requests != 1 || len(received) != 17449 {
		t.Fatal("timestamp transfer incomplete")
	}
	for i, b := range received {
		if b != byte(i*31+7) {
			t.Fatal("timestamp bytes changed")
		}
	}
}

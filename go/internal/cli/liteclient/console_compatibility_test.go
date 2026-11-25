package liteclient

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func TestConsoleAttachDiscardsUnsupportedOutputStreams(t *testing.T) {
	client, device := connectedOverPipe(t)
	_ = device.SetDeadline(time.Now().Add(5 * time.Second))
	stdout := []byte{0x00, 0xff, 'o', 0x10, '\n'}
	stderr := []byte{0xfe, 0x00, 'e', 0x80, '\r'}
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- func() error {
			link := newDirectLink(device)
			request, err := link.recv(time.Second)
			if err != nil {
				return err
			}
			cmd := request.GetCommand()
			if cmd.GetConsoleAttach() == nil {
				return fmt.Errorf("expected console attach, got %v", request)
			}
			eventID := cmd.GetConsoleAttach().GetEventId()
			reply := func(command *wendypb.WendyComCommand) error {
				return link.send(&wendypb.WendyComMessage{Msg: &wendypb.WendyComMessage_Response{
					Response: &wendypb.WendyComResponse{RequestId: command.GetRequestId()},
				}})
			}
			if err := reply(cmd); err != nil {
				return err
			}
			for _, data := range []*wendypb.WendyComConsoleData{
				{Io: wendypb.WendyComConsoleIo(-1), Data: []byte("future negative"), Gap: true},
				{Io: wendypb.WendyComConsoleIo(99), Data: []byte("future positive")},
				{Io: wendypb.WendyComConsoleIo_WENDY_COM_CONSOLE_IO_STANDARD_INPUT, Data: []byte("inbound only")},
				{Io: wendypb.WendyComConsoleIo_WENDY_COM_CONSOLE_IO_STANDARD_OUTPUT, Data: stdout, Gap: true},
				{Io: wendypb.WendyComConsoleIo_WENDY_COM_CONSOLE_IO_STANDARD_ERROR, Data: stderr},
			} {
				if err := link.send(&wendypb.WendyComMessage{Msg: &wendypb.WendyComMessage_Event{
					Event: &wendypb.WendyComEvent{EventId: eventID, Data: &wendypb.WendyComEvent_ConsoleData{ConsoleData: data}},
				}}); err != nil {
					return err
				}
			}
			for _, want := range []string{"ping", "detach"} {
				request, err = link.recv(time.Second)
				if err != nil {
					return err
				}
				cmd = request.GetCommand()
				if want == "ping" && cmd.GetPing() == nil || want == "detach" && cmd.GetConsoleDetach() == nil {
					return fmt.Errorf("expected %s, got %v", want, request)
				}
				if err := reply(cmd); err != nil {
					return err
				}
			}
			return nil
		}()
	}()

	chunks, detach, err := client.ConsoleAttach(false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = detach(true) })
	for i, want := range []ConsoleChunk{
		{Data: stdout, Gap: true},
		{Data: stderr, Stderr: true},
	} {
		select {
		case got, ok := <-chunks:
			if !ok || !bytes.Equal(got.Data, want.Data) || got.Gap != want.Gap || got.Stderr != want.Stderr {
				t.Fatalf("chunk %d = %+v (open=%v), want %+v", i, got, ok, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for known output chunk %d", i)
		}
	}
	if err := client.Ping(); err != nil {
		t.Fatalf("same connection after future output streams: %v", err)
	}
	if err := detach(false); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-chunks; ok {
		t.Fatal("unexpected extra output chunk")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

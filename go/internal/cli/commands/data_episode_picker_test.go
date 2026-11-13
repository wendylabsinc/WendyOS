package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/grpc"
)

// Three episodes as a device lists them, deliberately out of order. Two share
// a name, as repeated runs of one campaign do.
var (
	newestEpisode = &agentpbv2.DataEpisode{Id: "20261027T120235Z-2fb01a985660231d", Name: "c920-yolo-live", State: "complete", StartedUnixNanos: 1793102555554272224, SizeBytes: 4254620}
	middleEpisode = &agentpbv2.DataEpisode{Id: "20261027T115823Z-6c0c794daae03351", Name: "c920-yolo-live", State: "interrupted", StartedUnixNanos: 1793102303957931136, SizeBytes: 7827722}
	oldestEpisode = &agentpbv2.DataEpisode{Id: "20261027T115329Z-e4418826f638364e", Name: "manual-test", State: "complete", StartedUnixNanos: 1793102009610548480, SizeBytes: 7245349}
)

func unorderedEpisodes() []*agentpbv2.DataEpisode {
	return []*agentpbv2.DataEpisode{middleEpisode, oldestEpisode, newestEpisode}
}

// episodeListClient answers Episodes from a fixed list. Every other RPC is the
// embedded nil interface, so reaching one panics instead of passing silently.
type episodeListClient struct {
	agentpbv2.DataServiceClient
	episodes []*agentpbv2.DataEpisode
}

func (c *episodeListClient) Episodes(context.Context, *agentpbv2.DataEpisodesRequest, ...grpc.CallOption) (*agentpbv2.DataEpisodesResponse, error) {
	return &agentpbv2.DataEpisodesResponse{Episodes: c.episodes}, nil
}

type episodePickFunc func(string, []tui.PickerItem, []tui.PickerColumn) (string, error)

func stubEpisodePicker(t *testing.T, pick episodePickFunc) {
	t.Helper()
	prev := pickEpisodeFn
	pickEpisodeFn = pick
	t.Cleanup(func() { pickEpisodeFn = prev })
}

func neverPickEpisode(t *testing.T) episodePickFunc {
	return func(string, []tui.PickerItem, []tui.PickerColumn) (string, error) {
		t.Fatal("the episode picker opened")
		return "", nil
	}
}

// stubEpisodeTerminal sets what the command sees of its terminal and of --json,
// and clears any --device a previous test left behind.
func stubEpisodeTerminal(t *testing.T, terminal, json bool) {
	t.Helper()
	prevTerminal, prevJSON, prevDevice := isInteractiveTerminalFn, jsonOutput, deviceFlag
	isInteractiveTerminalFn = func() bool { return terminal }
	jsonOutput = json
	deviceFlag = ""
	t.Cleanup(func() { isInteractiveTerminalFn, jsonOutput, deviceFlag = prevTerminal, prevJSON, prevDevice })
}

// TestEpisodePickerShowsEpisodesNewestFirst drives the real picker model with
// the rows the command builds: newest first whatever order the device sent,
// one row per episode even when names repeat, every column filled, and Enter
// on the first row choosing the newest episode.
func TestEpisodePickerShowsEpisodesNewestFirst(t *testing.T) {
	var view string
	stubEpisodePicker(t, func(title string, items []tui.PickerItem, columns []tui.PickerColumn) (string, error) {
		if title != "Select an episode to download" {
			t.Errorf("picker title = %q", title)
		}
		var model tea.Model = tui.NewPickerWithTitleAndColumns(title, columns)
		for _, msg := range []tea.Msg{tea.WindowSizeMsg{Width: 200, Height: 40}, tui.PickerAddMsg{Items: items}, tui.PickerDoneMsg{}} {
			model, _ = model.Update(msg)
		}
		view = model.View()
		model, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		selected := model.(tui.PickerModel).Selected()
		if selected == nil {
			t.Fatal("Enter selected nothing")
		}
		return selected.Value.(string), nil
	})

	id, ok, err := resolveEpisodeArg(context.Background(), &episodeListClient{episodes: unorderedEpisodes()}, nil, &bytes.Buffer{}, "Select an episode to download")
	if err != nil || !ok {
		t.Fatalf("resolveEpisodeArg = %q, %v, %v", id, ok, err)
	}
	if id != newestEpisode.GetId() {
		t.Fatalf("Enter on the first row chose %q, want the newest episode %q", id, newestEpisode.GetId())
	}

	at := func(s string) int {
		i := strings.Index(view, s)
		if i < 0 {
			t.Fatalf("picker does not show %q:\n%s", s, view)
		}
		return i
	}
	if !(at(newestEpisode.GetId()) < at(middleEpisode.GetId()) && at(middleEpisode.GetId()) < at(oldestEpisode.GetId())) {
		t.Fatalf("episodes are not listed newest first:\n%s", view)
	}
	for _, want := range []string{
		"Episode", "Name", "State", "Size", "Started",
		"c920-yolo-live", "manual-test", "complete", "interrupted",
		formatBytes(newestEpisode.GetSizeBytes()),
		time.Unix(0, newestEpisode.GetStartedUnixNanos()).Local().Format("2006-01-02 15:04:05"),
	} {
		at(want)
	}
}

// TestResolveEpisodeArgReturnsThePickedRowsID checks the row's value is the
// episode ID, not its name, and that a cancelled picker reaches the caller.
func TestResolveEpisodeArgReturnsThePickedRowsID(t *testing.T) {
	client := &episodeListClient{episodes: unorderedEpisodes()}
	stubEpisodePicker(t, func(_ string, items []tui.PickerItem, _ []tui.PickerColumn) (string, error) {
		return items[1].Value.(string), nil
	})
	id, ok, err := resolveEpisodeArg(context.Background(), client, nil, &bytes.Buffer{}, "t")
	if err != nil || !ok || id != middleEpisode.GetId() {
		t.Fatalf("resolveEpisodeArg = %q, %v, %v; want %q", id, ok, err, middleEpisode.GetId())
	}

	stubEpisodePicker(t, func(string, []tui.PickerItem, []tui.PickerColumn) (string, error) { return "", ErrUserCancelled })
	if _, ok, err = resolveEpisodeArg(context.Background(), client, nil, &bytes.Buffer{}, "t"); ok || !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("cancelled picker = ok %v, err %v; want ErrUserCancelled", ok, err)
	}

	// A named episode is used as given, without listing or picking.
	stubEpisodePicker(t, neverPickEpisode(t))
	if id, ok, err = resolveEpisodeArg(context.Background(), &episodeListClient{}, []string{"ep-named"}, &bytes.Buffer{}, "t"); err != nil || !ok || id != "ep-named" {
		t.Fatalf("named episode = %q, %v, %v", id, ok, err)
	}
}

// fakeEpisodeAgent serves the data RPCs the episode commands use, each episode
// holding one file, and counts every call so a test can prove none was made.
type fakeEpisodeAgent struct {
	agentpbv2.UnimplementedDataServiceServer
	episodes []*agentpbv2.DataEpisode
	payload  []byte
	calls    atomic.Int32
}

func (f *fakeEpisodeAgent) manifest(id string) []byte {
	sum := sha256.Sum256(f.payload)
	return fmt.Appendf(nil, `{"id":%q,"state":"complete","files":[{"path":"events.jsonl","size":%d,"sha256":%q}]}`, id, len(f.payload), hex.EncodeToString(sum[:]))
}

func (f *fakeEpisodeAgent) Episodes(context.Context, *agentpbv2.DataEpisodesRequest) (*agentpbv2.DataEpisodesResponse, error) {
	f.calls.Add(1)
	return &agentpbv2.DataEpisodesResponse{Episodes: f.episodes}, nil
}

func (f *fakeEpisodeAgent) Inspect(_ context.Context, req *agentpbv2.DataInspectRequest) (*agentpbv2.DataInspectResponse, error) {
	f.calls.Add(1)
	return &agentpbv2.DataInspectResponse{ManifestJson: f.manifest(req.GetEpisode())}, nil
}

func (f *fakeEpisodeAgent) Download(req *agentpbv2.DataDownloadRequest, stream grpc.ServerStreamingServer[agentpbv2.DataDownloadChunk]) error {
	f.calls.Add(1)
	return stream.Send(&agentpbv2.DataDownloadChunk{Offset: req.GetOffset(), Data: f.payload[req.GetOffset():], Eof: true})
}

func (f *fakeEpisodeAgent) ExportRecording(*agentpbv2.DataRecordingExportRequest, grpc.ServerStreamingServer[recordingpb.StoredRecord]) error {
	f.calls.Add(1)
	return nil
}

// serveEpisodeAgent puts agent on WENDY_AGENT_SOCKET, the path every command
// dials before any discovery, so the real command runs end to end.
func serveEpisodeAgent(t *testing.T, agent *fakeEpisodeAgent) {
	t.Helper()
	startUDSAgentServing(t, versionOnlyAgent{}, func(s *grpc.Server) { agentpbv2.RegisterDataServiceServer(s, agent) })
}

func runDataCommand(t *testing.T, cmd *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	// As under the real root command, which owns error and usage printing.
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = cmd.ExecuteContext(ctx)
	return out.String(), errOut.String(), err
}

// TestDataDownloadPickedEpisodeReportsAbsolutePath is the case from the field:
// no episode ID in a terminal. The picker chooses, and the success line names
// the absolute directory instead of repeating the episode ID back.
func TestDataDownloadPickedEpisodeReportsAbsolutePath(t *testing.T) {
	stubEpisodeTerminal(t, true, false)
	agent := &fakeEpisodeAgent{episodes: unorderedEpisodes(), payload: []byte("{\"t\":1}\n")}
	serveEpisodeAgent(t, agent)
	stubEpisodePicker(t, func(_ string, items []tui.PickerItem, _ []tui.PickerColumn) (string, error) {
		return items[1].Value.(string), nil
	})
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runDataCommand(t, newDataDownloadCmd())
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	want := filepath.Join(cwd, middleEpisode.GetId())
	if stdout != fmt.Sprintf("Downloaded %s to %s\n", middleEpisode.GetId(), want) {
		t.Fatalf("stdout = %q, want the absolute destination %q", stdout, want)
	}
	if got, err := os.ReadFile(filepath.Join(want, "events.jsonl")); err != nil || !bytes.Equal(got, agent.payload) {
		t.Fatalf("downloaded file = %q, %v", got, err)
	}

	// The same episode again: the destination now exists, and the refusal
	// names it absolutely too.
	_, _, err = runDataCommand(t, newDataDownloadCmd())
	if err == nil || !strings.Contains(err.Error(), "destination already exists: "+want) {
		t.Fatalf("second download error = %v, want it to name %q", err, want)
	}
}

// TestDataDownloadNamedEpisodeReportsAbsolutePath covers -o with a relative
// path, which is reported resolved against the working directory.
func TestDataDownloadNamedEpisodeReportsAbsolutePath(t *testing.T) {
	stubEpisodeTerminal(t, false, false)
	agent := &fakeEpisodeAgent{payload: []byte("x")}
	serveEpisodeAgent(t, agent)
	stubEpisodePicker(t, neverPickEpisode(t))
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runDataCommand(t, newDataDownloadCmd(), "ep1", "-o", filepath.Join("out", "ep")+string(os.PathSeparator))
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if want := fmt.Sprintf("Downloaded ep1 to %s\n", filepath.Join(cwd, "out", "ep")); stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

// TestDataDownloadRefusesPickedIDThatIsNotADirectoryName: a picked ID comes
// from the device and, without -o, names the download directory. One that
// would land outside the working directory is refused before anything is
// fetched or written.
func TestDataDownloadRefusesPickedIDThatIsNotADirectoryName(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "work")
	if err := os.Mkdir(cwd, 0o750); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, id string }{
		{"parent", filepath.Join("..", "escaped")},
		{"absolute", filepath.Join(root, "escaped")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubEpisodeTerminal(t, true, false)
			agent := &fakeEpisodeAgent{episodes: []*agentpbv2.DataEpisode{{Id: tc.id, State: "complete", StartedUnixNanos: 1}}, payload: []byte("x")}
			serveEpisodeAgent(t, agent)
			stubEpisodePicker(t, func(_ string, items []tui.PickerItem, _ []tui.PickerColumn) (string, error) {
				return items[0].Value.(string), nil
			})
			t.Chdir(cwd)

			_, _, err := runDataCommand(t, newDataDownloadCmd())
			if err == nil || !strings.Contains(err.Error(), "not a plain directory name") {
				t.Fatalf("error = %v, want the picked ID refused", err)
			}
			if n := agent.calls.Load(); n != 1 {
				t.Errorf("made %d device calls, want only the episode list", n)
			}
			if entries, _ := os.ReadDir(root); len(entries) != 1 {
				t.Errorf("%s holds %d entries, want only the working directory", root, len(entries))
			}
			if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
				t.Errorf("wrote %d entries to the working directory", len(entries))
			}
		})
	}
}

// TestDataEpisodeCommandsRequireIDWithoutPicker: with no terminal, with
// --json, or with --non-interactive, a missing ID is an error that says why
// and where IDs come from, and nothing is asked of the device.
func TestDataEpisodeCommandsRequireIDWithoutPicker(t *testing.T) {
	cases := []struct {
		name     string
		terminal bool
		json     bool
		flags    []string
	}{
		{"no terminal", false, false, nil},
		{"json", true, true, nil},
		{"non-interactive flag", true, false, []string{"--non-interactive"}},
	}
	commands := []struct {
		verb string
		new  func() *cobra.Command
	}{
		{"download", newDataDownloadCmd},
		{"inspect", newDataInspectCmd},
	}
	for _, c := range commands {
		for _, tc := range cases {
			t.Run(c.verb+"/"+tc.name, func(t *testing.T) {
				stubEpisodeTerminal(t, tc.terminal, tc.json)
				agent := &fakeEpisodeAgent{episodes: unorderedEpisodes()}
				serveEpisodeAgent(t, agent)
				stubEpisodePicker(t, neverPickEpisode(t))
				t.Chdir(t.TempDir())

				stdout, _, err := runDataCommand(t, c.new(), tc.flags...)
				if err == nil {
					t.Fatal("ran without an episode ID")
				}
				if !strings.Contains(err.Error(), "episode ID is required in non-interactive or --json mode") {
					t.Errorf("error %q does not say why", err)
				}
				steps := strings.Join(NextSteps(err), "\n")
				for _, want := range []string{"wendy data episodes", "wendy data " + c.verb + " <episode>"} {
					if !strings.Contains(steps, want) {
						t.Errorf("next steps %q do not name %q", steps, want)
					}
				}
				// The argument check this replaces was a usage error (exit 2);
				// scripts relying on that status keep it.
				if !IsUsageError(err) {
					t.Errorf("error %v is not a usage error", err)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want nothing", stdout)
				}
				if n := agent.calls.Load(); n != 0 {
					t.Errorf("made %d device calls before refusing", n)
				}
			})
		}
	}
}

// TestDataEpisodeCommandsWithNoEpisodes prints a plain message rather than an
// empty picker, and downloads nothing.
func TestDataEpisodeCommandsWithNoEpisodes(t *testing.T) {
	for _, newCmd := range []func() *cobra.Command{newDataDownloadCmd, newDataInspectCmd} {
		cmd := newCmd()
		t.Run(cmd.Name(), func(t *testing.T) {
			stubEpisodeTerminal(t, true, false)
			serveEpisodeAgent(t, &fakeEpisodeAgent{})
			stubEpisodePicker(t, neverPickEpisode(t))
			dir := t.TempDir()
			t.Chdir(dir)

			stdout, _, err := runDataCommand(t, cmd)
			if err != nil {
				t.Fatalf("error = %v, want a plain message", err)
			}
			if stdout != "No finalized episodes on this device yet. Record one with 'wendy data record', or seal one in progress with 'wendy data stop'.\n" {
				t.Fatalf("stdout = %q", stdout)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("wrote %d entries to the working directory", len(entries))
			}
		})
	}
}

// TestDataInspectPickedEpisode checks inspect asks the device about the
// episode the picker chose.
func TestDataInspectPickedEpisode(t *testing.T) {
	stubEpisodeTerminal(t, true, false)
	serveEpisodeAgent(t, &fakeEpisodeAgent{episodes: unorderedEpisodes()})
	stubEpisodePicker(t, func(title string, items []tui.PickerItem, _ []tui.PickerColumn) (string, error) {
		if title != "Select an episode to inspect" {
			t.Errorf("picker title = %q", title)
		}
		return items[2].Value.(string), nil
	})

	stdout, _, err := runDataCommand(t, newDataInspectCmd())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !strings.HasPrefix(stdout, "Episode: "+oldestEpisode.GetId()+"\n") {
		t.Fatalf("stdout = %q, want the oldest episode's manifest", stdout)
	}
}

// TestDataExportStreamReportsAbsolutePath keeps export-stream's success line
// consistent with download's.
func TestDataExportStreamReportsAbsolutePath(t *testing.T) {
	stubEpisodeTerminal(t, false, false)
	serveEpisodeAgent(t, &fakeEpisodeAgent{})
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runDataCommand(t, newDataExportStreamCmd(), "app", "stream", "-o", "records.wdr")
	if err != nil {
		t.Fatalf("export-stream: %v", err)
	}
	if want := fmt.Sprintf("Exported 0 records to %s\n", filepath.Join(cwd, "records.wdr")); stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

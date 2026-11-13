package powermode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const orinConf = `# comment
< PARAM TYPE=FILE NAME=CPU_ONLINE >
< POWER_MODEL ID=0 NAME=MAXN >
CPU_ONLINE CORE_0 1
< POWER_MODEL ID=1 NAME=MODE_15W >
< POWER_MODEL ID=2 NAME=MODE_30W >
< POWER_MODEL ID=3 NAME=MODE_50W >

< PM_CONFIG DEFAULT=2 >
`

type call struct {
	stdin string
	args  []string
}

func fixture(t *testing.T, conf string, run func(c call) (string, error)) (*Controller, *[]call) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nvpmodel.conf")
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls []call
	return &Controller{
		ConfPath: path,
		Run: func(_ context.Context, stdin, name string, args ...string) (string, error) {
			if name != "nvpmodel" {
				t.Fatalf("ran %q, want nvpmodel", name)
			}
			c := call{stdin: stdin, args: args}
			calls = append(calls, c)
			return run(c)
		},
	}, &calls
}

func TestParseConfig(t *testing.T) {
	modes, def, err := ParseConfig(strings.NewReader(orinConf))
	if err != nil {
		t.Fatal(err)
	}
	want := []Mode{{0, "MAXN"}, {1, "MODE_15W"}, {2, "MODE_30W"}, {3, "MODE_50W"}}
	if len(modes) != len(want) {
		t.Fatalf("modes = %v, want %v", modes, want)
	}
	for i := range want {
		if modes[i] != want[i] {
			t.Errorf("modes[%d] = %v, want %v", i, modes[i], want[i])
		}
	}
	if def == nil || *def != 2 {
		t.Errorf("default = %v, want 2", def)
	}
}

func TestOutOfRangeIDsAreRejected(t *testing.T) {
	// 2^31 does not fit the wire's int32; it must error rather than wrap.
	if _, _, err := ParseConfig(strings.NewReader("< POWER_MODEL ID=2147483648 NAME=BIG >\n")); err == nil {
		t.Error("ParseConfig accepted an ID beyond int32")
	}
	if _, _, err := parseQuery("NV Power Mode: BIG\n2147483648\n"); err == nil {
		t.Error("parseQuery accepted an ID beyond int32")
	}
}

func TestParseQuery(t *testing.T) {
	id, name, err := parseQuery("NVPM WARN: something\nNV Power Mode: MODE_30W\n2\n")
	if err != nil || id != 2 || name != "MODE_30W" {
		t.Fatalf("got (%d, %q, %v)", id, name, err)
	}
	if _, _, err := parseQuery("garbage"); err == nil {
		t.Fatal("want error for unrecognised output")
	}
}

func TestState(t *testing.T) {
	c, calls := fixture(t, orinConf, func(call) (string, error) {
		return "NV Power Mode: MODE_30W\n2\n", nil
	})
	st, err := c.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != (Mode{2, "MODE_30W"}) {
		t.Errorf("current = %v", st.Current)
	}
	if len(st.Available) != 4 {
		t.Errorf("available = %v", st.Available)
	}
	if st.BootDefault == nil || st.BootDefault.Name != "MODE_30W" {
		t.Errorf("boot default = %v", st.BootDefault)
	}
	if len(*calls) != 1 || (*calls)[0].args[0] != "-q" {
		t.Errorf("calls = %v, want a single nvpmodel -q", *calls)
	}
}

func TestStateUnavailable(t *testing.T) {
	c := &Controller{ConfPath: filepath.Join(t.TempDir(), "missing.conf"), Run: execRunner}
	if _, err := c.State(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing conf: err = %v, want ErrUnavailable", err)
	}

	c, _ = fixture(t, orinConf, func(call) (string, error) { return "", exec.ErrNotFound })
	if _, err := c.State(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing binary: err = %v, want ErrUnavailable", err)
	}

	c, _ = fixture(t, "# no modes\n", func(call) (string, error) { return "", nil })
	if _, err := c.State(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty conf: err = %v, want ErrUnavailable", err)
	}
}

func TestResolve(t *testing.T) {
	modes, _, _ := ParseConfig(strings.NewReader(orinConf))
	for query, want := range map[string]int32{"MAXN": 0, "0": 0, "mode_50w": 3, " 2 ": 2, "MODE_15W": 1} {
		got, err := Resolve(modes, query)
		if err != nil || got.ID != want {
			t.Errorf("Resolve(%q) = (%v, %v), want id %d", query, got, err, want)
		}
	}
	for _, query := range []string{"", "9", "turbo"} {
		if _, err := Resolve(modes, query); !errors.Is(err, ErrUnknownMode) {
			t.Errorf("Resolve(%q) err = %v, want ErrUnknownMode", query, err)
		}
	}
}

func TestSetLive(t *testing.T) {
	c, calls := fixture(t, orinConf, func(call) (string, error) { return "NVPM: switched\n", nil })
	res, err := c.Set(context.Background(), "MODE_15W", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != (Mode{1, "MODE_15W"}) || res.RebootRequired {
		t.Errorf("result = %+v", res)
	}
	got := (*calls)[0]
	if strings.Join(got.args, " ") != "-m 1" || got.stdin != "n\n" {
		t.Errorf("call = %+v, want `-m 1` with stdin \"n\\n\"", got)
	}
}

func TestSetRebootRequired(t *testing.T) {
	// nvpmodel may exit non-zero after the prompt is declined; the message, not
	// the exit status, says a reboot is needed.
	c, _ := fixture(t, orinConf, func(call) (string, error) {
		return "Reboot required for changing to this power mode: 0\nReboot now? (Y/N) ", errors.New("exit status 1")
	})
	res, err := c.Set(context.Background(), "0", false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.RebootRequired || res.Mode.Name != "MAXN" {
		t.Errorf("result = %+v", res)
	}
}

func TestSetRebootAnswersTheNvpmodelPromptYes(t *testing.T) {
	c, calls := fixture(t, orinConf, func(call) (string, error) {
		return "Reboot required for changing to this power mode: 0\n", nil
	})
	res, err := c.Set(context.Background(), "MAXN", true)
	if err != nil || !res.RebootRequired {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if got := (*calls)[0]; got.stdin != "y\n" || strings.Join(got.args, " ") != "-m 0" {
		t.Errorf("call = %+v, want `-m 0` answered y", got)
	}
}

func TestSetFailure(t *testing.T) {
	c, _ := fixture(t, orinConf, func(call) (string, error) { return "NVPM ERROR: boom\n", errors.New("exit status 255") })
	if _, err := c.Set(context.Background(), "MAXN", false); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want one carrying nvpmodel's output", err)
	}
}

func TestSetUnknownModeRunsNothing(t *testing.T) {
	c, calls := fixture(t, orinConf, func(call) (string, error) { return "", nil })
	if _, err := c.Set(context.Background(), "turbo", false); !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("err = %v, want ErrUnknownMode", err)
	}
	if len(*calls) != 0 {
		t.Errorf("nvpmodel ran for an unknown mode: %v", *calls)
	}
}

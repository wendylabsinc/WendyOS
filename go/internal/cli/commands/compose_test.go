package commands

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func writeComposeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseComposeFile_PrefersDockerCompose(t *testing.T) {
	dir := t.TempDir()
	writeComposeFile(t, dir, "compose.yml", "services:\n  reporter:\n    image: alpine\n")
	writeComposeFile(t, dir, "docker-compose.yml", "services:\n  greeter:\n    image: alpine\n")

	cfg, name, err := parseComposeFile(dir)
	if err != nil {
		t.Fatalf("parseComposeFile: %v", err)
	}
	if name != "docker-compose.yml" {
		t.Fatalf("expected docker-compose.yml, got %q", name)
	}
	if _, ok := cfg.Services["greeter"]; !ok {
		t.Fatalf("expected service 'greeter', got %#v", cfg.Services)
	}
}

func TestParseComposeFile_NotFound(t *testing.T) {
	if _, _, err := parseComposeFile(t.TempDir()); err == nil {
		t.Fatal("expected error when no compose file is present")
	}
}

func TestComposeBuildContext(t *testing.T) {
	parse := func(t *testing.T, body string) composeService {
		t.Helper()
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Services["svc"]
	}

	t.Run("scalar build path", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    build: ./api\n")
		ctxDir, df, args, err := composeBuildContext(svc, "/proj")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ctxDir != "/proj/api" || df != "Dockerfile" || args != nil {
			t.Fatalf("got (%q,%q,%v)", ctxDir, df, args)
		}
	})

	t.Run("mapping with custom dockerfile and args", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    build:\n      context: ./svc\n      dockerfile: Dockerfile.dev\n      args:\n        FOO: bar\n")
		ctxDir, df, args, err := composeBuildContext(svc, "/proj")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ctxDir != "/proj/svc" || df != "Dockerfile.dev" || args["FOO"] != "bar" {
			t.Fatalf("got (%q,%q,%v)", ctxDir, df, args)
		}
	})

	t.Run("missing build returns empty", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    image: alpine\n")
		ctxDir, df, _, err := composeBuildContext(svc, "/proj")
		if err != nil || ctxDir != "" || df != "" {
			t.Fatalf("got (%q,%q,err=%v)", ctxDir, df, err)
		}
	})

	t.Run("unsupported build kind errors", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    build: [./a, ./b]\n")
		if _, _, _, err := composeBuildContext(svc, "/proj"); err == nil {
			t.Fatal("expected error for sequence build kind")
		}
	})
}

func TestComposeEnv(t *testing.T) {
	parse := func(t *testing.T, body string) composeService {
		t.Helper()
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Services["svc"]
	}

	t.Run("mapping with mixed types", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    environment:\n      STR: hello\n      NUM: 42\n      BOOL: true\n")
		got := composeEnv(svc)
		sort.Strings(got)
		want := []string{"BOOL=true", "NUM=42", "STR=hello"}
		if len(got) != len(want) {
			t.Fatalf("got %v want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	})

	t.Run("mapping null inherits from process env", func(t *testing.T) {
		t.Setenv("WENDY_TEST_INHERIT", "from-process")
		svc := parse(t, "services:\n  svc:\n    environment:\n      WENDY_TEST_INHERIT: ~\n")
		got := composeEnv(svc)
		if len(got) != 1 || got[0] != "WENDY_TEST_INHERIT=from-process" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("list KEY entries inherit", func(t *testing.T) {
		t.Setenv("WENDY_TEST_LIST", "from-list")
		svc := parse(t, "services:\n  svc:\n    environment:\n      - WENDY_TEST_LIST\n      - EXPLICIT=value\n")
		got := composeEnv(svc)
		sort.Strings(got)
		want := []string{"EXPLICIT=value", "WENDY_TEST_LIST=from-list"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("list KEY without process env is dropped", func(t *testing.T) {
		os.Unsetenv("WENDY_TEST_MISSING")
		svc := parse(t, "services:\n  svc:\n    environment:\n      - WENDY_TEST_MISSING\n")
		if got := composeEnv(svc); len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
}

func TestServiceOrder(t *testing.T) {
	parseConfig := func(t *testing.T, body string) *composeConfig {
		t.Helper()
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		return &cfg
	}

	t.Run("respects depends_on list form", func(t *testing.T) {
		cfg := parseConfig(t, "services:\n  api:\n    image: a\n    depends_on:\n      - db\n  db:\n    image: b\n")
		ordered, err := serviceOrder(cfg)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		// db must precede api.
		dbIdx, apiIdx := -1, -1
		for i, n := range ordered {
			if n == "db" {
				dbIdx = i
			}
			if n == "api" {
				apiIdx = i
			}
		}
		if dbIdx == -1 || apiIdx == -1 || dbIdx > apiIdx {
			t.Fatalf("expected db before api, got %v", ordered)
		}
	})

	t.Run("respects depends_on map form", func(t *testing.T) {
		cfg := parseConfig(t, "services:\n  api:\n    image: a\n    depends_on:\n      db:\n        condition: service_started\n  db:\n    image: b\n")
		ordered, err := serviceOrder(cfg)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(ordered) != 2 {
			t.Fatalf("got %v", ordered)
		}
	})

	t.Run("rejects unknown dependency", func(t *testing.T) {
		cfg := parseConfig(t, "services:\n  api:\n    image: a\n    depends_on:\n      - nope\n")
		if _, err := serviceOrder(cfg); err == nil {
			t.Fatal("expected error for unknown dependency")
		}
	})
}

func TestParseComposeVolume(t *testing.T) {
	cases := []struct {
		in       string
		src, tgt string
	}{
		{"data:/var/lib", "data", "/var/lib"},
		{"data:/var/lib:ro", "data", "/var/lib"},
		{"./host:/in/container", "./host", "/in/container"},
		{"/abs/host:/in/container", "/abs/host", "/in/container"},
		{"anonymous", "", "anonymous"},
		{"C:\\Users\\foo:/data", "C:\\Users\\foo", "/data"},
	}
	for _, c := range cases {
		src, tgt, _ := parseComposeVolume(c.in)
		if src != c.src || tgt != c.tgt {
			t.Errorf("parseComposeVolume(%q) = (%q,%q); want (%q,%q)", c.in, src, tgt, c.src, c.tgt)
		}
	}
}

func TestComposeAppConfig(t *testing.T) {
	parse := func(t *testing.T, body string) composeService {
		t.Helper()
		var cfg composeConfig
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Services["svc"]
	}

	t.Run("ports synthesise network entitlement", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    ports:\n      - \"8080:80\"\n      - \"9000\"\n")
		cfg := composeAppConfig("proj", "svc", svc)
		if cfg.AppID != "proj-svc" {
			t.Fatalf("appID: %s", cfg.AppID)
		}
		var ports []appconfig.PortMapping
		for _, e := range cfg.Entitlements {
			if e.Type == appconfig.EntitlementNetwork {
				ports = e.Ports
			}
		}
		if len(ports) != 2 || ports[0].Host != 8080 || ports[0].Container != 80 || ports[1].Host != 9000 || ports[1].Container != 9000 {
			t.Fatalf("unexpected ports: %+v", ports)
		}
	})

	t.Run("network_mode host overrides ports", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    network_mode: host\n    ports:\n      - \"80:80\"\n")
		cfg := composeAppConfig("proj", "svc", svc)
		var found bool
		for _, e := range cfg.Entitlements {
			if e.Type == appconfig.EntitlementNetwork && e.Mode == "host" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected host-mode network entitlement, got %+v", cfg.Entitlements)
		}
	})

	t.Run("named volumes become persist entitlements; bind mounts skipped", func(t *testing.T) {
		svc := parse(t, "services:\n  svc:\n    volumes:\n      - data:/var/lib\n      - ./host:/in/container\n      - /abs/host:/in/container\n      - cache:/cache:ro\n")
		cfg := composeAppConfig("proj", "svc", svc)
		var persists []appconfig.Entitlement
		for _, e := range cfg.Entitlements {
			if e.Type == appconfig.EntitlementPersist {
				persists = append(persists, e)
			}
		}
		if len(persists) != 2 {
			t.Fatalf("want 2 persist entitlements, got %d: %+v", len(persists), persists)
		}
		names := map[string]string{persists[0].Name: persists[0].Path, persists[1].Name: persists[1].Path}
		if names["data"] != "/var/lib" || names["cache"] != "/cache" {
			t.Fatalf("unexpected persist mapping: %+v", names)
		}
	})
}

func TestNormalizeImageRef(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Bare name → docker.io/library/<name>:latest.
		{"python", "docker.io/library/python:latest"},
		// Name + tag.
		{"python:3.11-slim", "docker.io/library/python:3.11-slim"},
		// Two-segment ref → docker.io/<org>/<name>.
		{"library/nginx:1.27", "docker.io/library/nginx:1.27"},
		{"bitnami/redis:7", "docker.io/bitnami/redis:7"},
		// Custom registry passes through.
		{"gcr.io/google-containers/pause:3.9", "gcr.io/google-containers/pause:3.9"},
		{"localhost:5000/foo:bar", "localhost:5000/foo:bar"},
		{"registry.example.com:5000/team/app:1.2.3", "registry.example.com:5000/team/app:1.2.3"},
		// Digest references.
		{"python@sha256:0000000000000000000000000000000000000000000000000000000000000000", "docker.io/library/python@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		// Whitespace is trimmed.
		{"  python:3.11-slim  ", "docker.io/library/python:3.11-slim"},
		// Malformed → original input.
		{"this is not a ref", "this is not a ref"},
	}
	for _, c := range cases {
		if got := normalizeImageRef(c.in); got != c.want {
			t.Errorf("normalizeImageRef(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestComposeArgv_PreservesMultiLineScript(t *testing.T) {
	body := "services:\n  svc:\n    command:\n      - python3\n      - -c\n      - |\n        import sys\n        print('hello')\n"
	var cfg composeConfig
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	cmd, extra := composeArgv(cfg.Services["svc"])
	if cmd != "python3" {
		t.Fatalf("cmd = %q; want python3", cmd)
	}
	if len(extra) != 2 || extra[0] != "-c" {
		t.Fatalf("extra = %v", extra)
	}
	// The script body must survive intact, with its embedded newlines, so
	// the agent's strings.Fields(cmd) split can't word-split it.
	if !strings.Contains(extra[1], "import sys") || !strings.Contains(extra[1], "print('hello')") {
		t.Fatalf("script body lost; got %q", extra[1])
	}
}

func TestComposeArgv_ScalarShellSplit(t *testing.T) {
	body := "services:\n  svc:\n    command: \"python3 -m pip install -r requirements.txt\"\n"
	var cfg composeConfig
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	cmd, extra := composeArgv(cfg.Services["svc"])
	want := []string{"-m", "pip", "install", "-r", "requirements.txt"}
	if cmd != "python3" || !equalStrings(extra, want) {
		t.Fatalf("cmd=%q extra=%v; want python3 + %v", cmd, extra, want)
	}
}

func TestComposeArgv_Empty(t *testing.T) {
	body := "services:\n  svc:\n    image: alpine\n"
	var cfg composeConfig
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	cmd, extra := composeArgv(cfg.Services["svc"])
	if cmd != "" || extra != nil {
		t.Fatalf("expected empty argv, got cmd=%q extra=%v", cmd, extra)
	}
}

func TestShellSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a b c", []string{"a", "b", "c"}},
		{"  spaces   between  ", []string{"spaces", "between"}},
		{`echo "hello world"`, []string{"echo", "hello world"}},
		{`echo 'single quotes work too'`, []string{"echo", "single quotes work too"}},
		{`mix "double" 'single' bare`, []string{"mix", "double", "single", "bare"}},
		{"", nil},
	}
	for _, c := range cases {
		got := shellSplit(c.in)
		if !equalStrings(got, c.want) {
			t.Errorf("shellSplit(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

func TestComposeRestartPolicy(t *testing.T) {
	cases := []struct {
		in   string
		want agentpb.RestartPolicyMode
	}{
		{"always", agentpb.RestartPolicyMode_UNLESS_STOPPED},
		{"unless-stopped", agentpb.RestartPolicyMode_UNLESS_STOPPED},
		{"on-failure", agentpb.RestartPolicyMode_ON_FAILURE},
		{"no", agentpb.RestartPolicyMode_NO},
		{"", agentpb.RestartPolicyMode_NO},
		{"weird", agentpb.RestartPolicyMode_DEFAULT},
	}
	for _, c := range cases {
		got := composeRestartPolicy(c.in).GetMode()
		if got != c.want {
			t.Errorf("composeRestartPolicy(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

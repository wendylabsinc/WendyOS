package codegen

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/recipe"
)

// GenerateGraph renders a lowered semantic graph as Dockerfile text. Generate
// remains as the reference renderer while differential tests keep this
// production path byte-identical across the full Stagefile feature set.
func GenerateGraph(g *ir.Graph, images map[string]string) (string, error) {
	if g == nil {
		return "", fmt.Errorf("nil graph")
	}
	var blocks []string
	start := 0
	for si, st := range g.Stages {
		if st.Final < start || st.Final >= len(g.Nodes) {
			return "", fmt.Errorf("stage %q: final node %d is outside the graph's %d nodes", st.Name, st.Final, len(g.Nodes))
		}
		var lines []string
		var platform string
		for i := start; i <= st.Final; i++ {
			n := g.Nodes[i]
			switch n.Kind {
			case ir.OpImage:
				if n.Image == nil {
					return "", fmt.Errorf("stage %q: node %d has nil image payload", st.Name, i)
				}
				platform = n.Image.Platform
				part, err := graphImageLines(g, n, st.Name, images)
				if err != nil {
					return "", fmt.Errorf("stage %q: %w", st.Name, err)
				}
				lines = append(lines, part...)
			case ir.OpFetch:
				if n.Fetch == nil {
					return "", fmt.Errorf("stage %q: node %d has nil fetch payload", st.Name, i)
				}
				fetch, err := recipe.FetchFor(n.Fetch)
				if err != nil {
					return "", fmt.Errorf("stage %q: %w", st.Name, err)
				}
				lines = append(lines, graphFetchInstruction(&fetch))
			case ir.OpExec:
				if n.Exec == nil {
					return "", fmt.Errorf("stage %q: node %d has nil exec payload", st.Name, i)
				}
				part, err := graphExecLines(n.Exec, platform)
				if err != nil {
					return "", fmt.Errorf("stage %q: %w", st.Name, err)
				}
				lines = append(lines, part...)
			case ir.OpCopy:
				if n.Copy == nil {
					return "", fmt.Errorf("stage %q: node %d has nil copy payload", st.Name, i)
				}
				line, err := graphCopyLine(g, n)
				if err != nil {
					return "", fmt.Errorf("stage %q: %w", st.Name, err)
				}
				lines = append(lines, line)
			default:
				return "", fmt.Errorf("stage %q: unhandled node kind %q", st.Name, n.Kind)
			}
		}
		if si == len(g.Stages)-1 {
			if st.Healthcheck != nil {
				lines = append(lines, graphHealthcheckLine(st.Healthcheck))
			}
			if st.Entrypoint != nil {
				lines = append(lines, "ENTRYPOINT "+graphJSONArgv(st.Entrypoint))
			}
			if len(st.Cmd) > 0 {
				lines = append(lines, "CMD "+graphJSONArgv(st.Cmd))
			}
			user := st.User
			if user == "" {
				user = ir.DefaultUser
			}
			lines = append(lines, "USER "+user)
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
		start = st.Final + 1
	}
	return strings.Join(blocks, "\n\n") + "\n", nil
}

func graphImageLines(g *ir.Graph, n ir.Node, name string, images map[string]string) ([]string, error) {
	im := n.Image
	digest := ""
	ref := im.Ref
	if im.FromStage {
		if len(n.Inputs) != 1 {
			return nil, fmt.Errorf("stage-derived image has %d inputs, want one", len(n.Inputs))
		}
		ref = ""
		for _, stage := range g.Stages {
			if stage.Final == n.Inputs[0] {
				ref = stage.Name
				break
			}
		}
		if ref == "" {
			return nil, fmt.Errorf("image source node %d is not a stage final", n.Inputs[0])
		}
	} else if !im.Unpinned {
		var ok bool
		digest, ok = images[im.Ref]
		if !ok || digest == "" {
			return nil, fmt.Errorf("no resolved digest for %q; run `stagefile lock`", im.Ref)
		}
	}
	lines := []string{graphFromLine(ref, digest, name, im.Platform)}
	lines = append(lines, graphKVLines("ARG", im.Args)...)
	lines = append(lines, graphKVLines("ENV", im.Env)...)
	if im.Workdir != "" {
		lines = append(lines, "WORKDIR "+im.Workdir)
	}
	return lines, nil
}

func graphExecLines(x *ir.ExecOp, platform string) ([]string, error) {
	steps, err := recipe.For(x, platform)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, step := range steps {
		switch {
		case step.Fetch != nil:
			lines = append(lines, graphFetchInstruction(step.Fetch))
		case step.Run != nil:
			if p := step.Run.PreCopy; p != nil {
				lines = append(lines, fmt.Sprintf("COPY %s %s", strings.Join(p.Paths, " "), p.Dest))
			}
			lines = append(lines, graphRunInstruction(step.Run))
		default:
			return nil, fmt.Errorf("recipe step for %q is neither a run nor a fetch", x.Recipe.Name)
		}
	}
	return lines, nil
}

func graphRunInstruction(r *recipe.RunSpec) string {
	var parts []string
	for _, m := range r.CacheMounts {
		spec := "type=cache"
		if m.Locked {
			spec += ",sharing=locked"
		}
		if m.ID != "" {
			spec += ",id=" + m.ID
		}
		parts = append(parts, "--mount="+spec+",target="+m.Dir)
	}
	command := strings.Join(r.Command, " \\\n    && ")
	if len(parts) <= 1 {
		return "RUN " + strings.Join(parts, "") + func() string {
			if len(parts) > 0 {
				return " "
			}
			return ""
		}() + command
	}
	return "RUN " + strings.Join(append(parts, command), " \\\n    ")
}

func graphFetchInstruction(f *recipe.Fetch) string {
	flags := ""
	if f.Owner != "" {
		flags += "--chown=" + f.Owner + " "
	}
	if f.Mode != "" {
		flags += "--chmod=" + f.Mode + " "
	}
	return fmt.Sprintf("ADD %s--checksum=%s %s %s", flags, f.Checksum, f.URL, f.Dest)
}

func graphCopyLine(g *ir.Graph, n ir.Node) (string, error) {
	if n.Copy.Dest == "" {
		return "", fmt.Errorf("copy node has an empty destination")
	}
	flags := ""
	if n.Copy.Link {
		flags += "--link "
	}
	if !n.Copy.FromLocal {
		if len(n.Inputs) < 2 {
			return "", fmt.Errorf("copy node has %d inputs, want base and source stage", len(n.Inputs))
		}
		from := ""
		for _, st := range g.Stages {
			if st.Final == n.Inputs[1] {
				from = st.Name
				break
			}
		}
		if from == "" {
			return "", fmt.Errorf("copy source node %d is not a stage final", n.Inputs[1])
		}
		flags += "--from=" + from + " "
	}
	if n.Copy.Owner != "" {
		flags += "--chown=" + n.Copy.Owner + " "
	}
	if n.Copy.Mode != "" {
		flags += "--chmod=" + n.Copy.Mode + " "
	}
	return fmt.Sprintf("COPY %s%s %s", flags, strings.Join(n.Copy.Paths, " "), n.Copy.Dest), nil
}

func graphFromLine(image, digest, name, platform string) string {
	if digest != "" {
		image += "@" + digest
	}
	if platform != "" {
		platform = "--platform=" + platform + " "
	}
	return fmt.Sprintf("FROM %s%s AS %s", platform, image, name)
}

func graphKVLines(instruction string, values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		if instruction == "ARG" && values[key] == "" {
			lines = append(lines, "ARG "+key)
		} else {
			lines = append(lines, fmt.Sprintf("%s %s=%s", instruction, key, strconv.Quote(values[key])))
		}
	}
	return lines
}

func graphJSONArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, value := range argv {
		quoted[i] = strconv.Quote(value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func graphHealthcheckLine(h *ir.Healthcheck) string {
	parts := []string{"HEALTHCHECK"}
	if h.Interval != "" {
		parts = append(parts, "--interval="+h.Interval)
	}
	if h.Timeout != "" {
		parts = append(parts, "--timeout="+h.Timeout)
	}
	if h.StartPeriod != "" {
		parts = append(parts, "--start-period="+h.StartPeriod)
	}
	if h.Retries > 0 {
		parts = append(parts, fmt.Sprintf("--retries=%d", h.Retries))
	}
	return strings.Join(append(parts, "CMD", graphJSONArgv(h.Exec)), " ")
}

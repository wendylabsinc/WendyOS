package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolkit"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwatch"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func kitCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("kit requires prepare, inspect or claim")
	}
	flags := flag.NewFlagSet("kit "+args[0], flag.ContinueOnError)
	catalog := flags.String("catalog", "", "owner-private local catalog")
	id := flags.String("id", "", "prepared kit identifier")
	output := flags.String("out", "", "new activation output")
	name := flags.String("name", "Offline office patrol", "operator label")
	source := flags.String("source", "", "pinned controller source")
	assets := flags.String("assets", "", "verified pinned assets")
	office := flags.String("office", "", "original office composition input")
	viewer := flags.String("viewer", "", "installed viewer adapter")
	physicsImage := flags.String("physics-image", "", "cached exact physics image ID")
	navImage := flags.String("navigation-image", "", "cached exact navigation image ID")
	physics := flags.String("physics", "", "stationary preparation physics origin")
	nav := flags.String("navigation", "", "stationary preparation navigation origin")
	schedulePath := flags.String("schedule", "", "reviewed schedule")
	duration := flags.Int("duration", 3600, "original approval lifetime from preparation, seconds")
	if e := flags.Parse(args[1:]); e != nil {
		return e
	}
	if *catalog == "" || flags.NArg() != 0 {
		return errors.New("--catalog required; positional arguments refused")
	}
	c, e := patrolkit.Open(*catalog)
	if e != nil {
		return e
	}
	defer c.Close()
	var result any
	switch args[0] {
	case "inspect":
		result, e = c.Inspect(time.Now())
	case "claim":
		if *output == "" {
			return errors.New("fresh --out required")
		}
		result, e = c.Claim(*id, *output, time.Now())
	case "prepare":
		if *duration < 1 || *duration > 86400 {
			return errors.New("duration must be 1..86400 seconds")
		}
		var token [12]byte
		if _, e = rand.Read(token[:]); e != nil {
			return e
		}
		kitID := "kit-" + hex.EncodeToString(token[:])
		runID := "patrol-" + kitID
		a := &adapter{physics: *physics, nav: *nav, client: &http.Client{Timeout: 12 * time.Second}}
		var state struct {
			RunID       string         `json:"run_id"`
			ModelSHA256 string         `json:"model_sha256"`
			Frame       world.FrameRef `json:"frame"`
		}
		if e = a.request(*physics, "/state", nil, &state); e != nil {
			return e
		}
		var sample patrolwork.Sample
		if e = a.request(*physics, "/work", nil, &sample); e != nil {
			return e
		}
		if sample.Schema != patrolwork.Schema || sample.Method != patrolwork.Method || sample.Sequence == 0 || sample.SimulationNS <= 0 || sample.RunID != state.RunID || sample.ModelSHA256 != state.ModelSHA256 || sample.BootID == "" || !sample.Valid || time.Now().UnixMilli()-sample.CapturedUnixMS > 1000 || sample.CapturedUnixMS > time.Now().UnixMilli() {
			return errors.New("preparation physics identity unavailable")
		}
		var candidates struct {
			Rooms []patrol.Room `json:"rooms"`
		}
		if e = a.request(*nav, "/candidates", nil, &candidates); e != nil {
			return e
		}
		if len(candidates.Rooms) == 0 {
			return errors.New("no prepared rooms")
		}
		schedule, e := readPatrolConfig(*schedulePath, candidates.Rooms)
		if e != nil {
			return e
		}
		if schedule.WorldWatch != nil {
			kitID, runID, e = watchKitIdentity(schedule.WorldWatch, state.RunID)
			if e != nil {
				return e
			}
		}
		bundle, e := patrolkit.BuildPackage(*source, *assets, *office, state.Frame, kitID)
		if e != nil {
			return e
		}
		executable, e := os.Executable()
		if e != nil {
			return e
		}
		binary, e := patrolkit.ReadRegular(executable, patrolkit.MaxRuntimeBytes)
		if e != nil {
			return e
		}
		script, e := patrolkit.ReadRegular(*viewer, 1<<20)
		if e != nil {
			return e
		}
		runtime := patrolkit.Runtime{PhysicsImage: *physicsImage, NavigationImage: *navImage, ExecutorSHA256: patrolkit.Digest(binary), ViewerSHA256: patrolkit.Digest(script)}
		if e = runtime.Validate(); e != nil {
			return e
		}
		schedule.Offline = &tasking.PatrolOffline{KitID: kitID, RunID: runID, PackageSHA256: bundle.Manifest.SHA256, ModelSHA256: state.ModelSHA256, RuntimeSHA256: runtime.Digest(), PreparationBootID: sample.BootID}
		prepared := time.Now().UnixMilli()
		approval, spec, e := signFixture(runID, state.Frame, candidates.Rooms, *duration, schedule)
		if e != nil {
			return e
		}
		profile := patrolkit.Profile{Schema: patrolkit.Schema, ID: kitID, Name: *name, RunID: runID, PackageSHA256: bundle.Manifest.SHA256, ModelSHA256: state.ModelSHA256, PreparationBootID: sample.BootID, Frame: state.Frame, Runtime: runtime, PreparedUnixMS: prepared, ExpiresUnixMS: spec.NotAfterUnixMS}
		result, e = c.Register(profile, bundle, approval, binary, script)
	default:
		return fmt.Errorf("unknown kit action %q", args[0])
	}
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func activation(catalog, id, directory string) (patrolkit.Verified, patrolkit.Claim, error) {
	var empty patrolkit.Verified
	var zero patrolkit.Claim
	if catalog == "" || id == "" {
		return empty, zero, errors.New("offline activation requires catalog and kit identifier")
	}
	c, e := patrolkit.Open(catalog)
	if e != nil {
		return empty, zero, e
	}
	defer c.Close()
	exe, e := os.Executable()
	if e != nil {
		return empty, zero, e
	}
	exe, e = filepath.EvalSymlinks(exe)
	if e != nil {
		return empty, zero, e
	}
	return c.Activation(id, directory, exe, time.Now())
}

// A prepared World watch must already be commissioned for its future identity.
// This never edits a contract, sample, grant or clock to fit a newly made kit.
func watchKitIdentity(p *patrolwatch.Policy, preparationRun string) (string, string, error) {
	run := p.Contract.Field.Simulation.RunID
	if run == preparationRun || !regexp.MustCompile(`^patrol-kit-[a-f0-9]{24}$`).MatchString(run) {
		return "", "", errors.New("offline World watch requires an explicitly commissioned future patrol-kit identity distinct from the preparation run; observations cannot be relabeled")
	}
	return strings.TrimPrefix(run, "patrol-"), run, nil
}

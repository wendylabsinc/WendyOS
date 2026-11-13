//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

var guideHeadingStyle = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
var guideActionStyle = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorInfo)
var errTourGuideBack = errors.New("back to previous tour menu")

var (
	tourGuidePick              = pickTourGuide
	tourGuideWait              = waitTourGuide
	tourGuideWalk              = walkTourRobot
	tourGuideBrowser           = browserOpen
	tourGuideScaffold          = scaffoldTourApp
	tourGuidePrepareDeployment = prepareTourAppDeployment
	tourGuideDeploy            = func(ctx context.Context, app *tourAppDeployment) error { return app.deploy(ctx) }
	tourGuideCleanup           = func(ctx context.Context, app *tourAppDeployment) error { return app.cleanup(ctx) }
	tourGuideControlApp        = controlTourApp
	tourGuideAppURL            = func(ctx context.Context, app *tourAppDeployment) (string, error) { return app.url(ctx) }
	tourGuideEditor            = openTourEditor
	tourGuideStop              = func(ctx context.Context, name string) error {
		store, err := robotVMStore()
		if err != nil {
			return err
		}
		return store.StopContext(ctx, name, false, vmStopGrace)
	}
)

func guideItem(order, name, description, value string) tui.PickerItem {
	return tui.PickerItem{SortKey: order, Name: name, Description: description, Value: value}
}

func runTourGuide(ctx context.Context, cmd *cobra.Command, p tourRobotProject, path string, port int, profile vm.RobotProfile) error {
	out := cmd.OutOrStdout()
	if !isInteractiveTerminalFn() {
		fmt.Fprintf(out, "Edit %s, then run %s.\nStop the simulator: wendy vm stop %s\n", path, tourRobotResumeCommand(path), p.VMName)
		return nil
	}
	apps := make(map[string]string)
	for {
		fmt.Fprintln(out, "\n"+guideHeadingStyle.Render("What will you teach your robot?"))
		action, err := tourGuidePick(ctx, out, "Choose your next step", []tui.PickerItem{
			guideItem("0", "Build a robot app", "Edit Python, deploy it here, and try your idea", "app"),
			guideItem("1", "Change the walk", "Try a straight line or turn the other way", "walk"),
			guideItem("2", "Drive in the browser", "Use the viewer's keyboard controls", "browser"),
			guideItem("3", "Finish for now", "Leave the simulator running or stop it", "finish"),
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errTourGuideBack) {
			guideResume(out, path, p.VMName, false)
			return nil
		}
		if err != nil {
			return err
		}
		switch action {
		case "app":
			if err := runTourAppGuide(ctx, out, p.VMName, filepath.Dir(path), apps); err != nil {
				return err
			}
		case "walk":
			if err := runTourWalkGuide(ctx, out, &p, path, port, profile); err != nil {
				return err
			}
		case "browser":
			fmt.Fprintln(out, "\n"+guideHeadingStyle.Render("Drive your Go2"))
			fmt.Fprintf(out, "%s, then hold %s to walk. %s stops; %s gives control back.\nViewer: %s\n", guideActionStyle.Render("Enable controls"), guideActionStyle.Render("W"), guideActionStyle.Render("Space"), guideActionStyle.Render("Release controls"), robotURL(port))
			_ = tourGuideBrowser(robotURL(port))
			if err := tourGuideWait(ctx, out, "Release controls in the viewer, then press Enter to continue."); err != nil && !errors.Is(err, errTourGuideBack) {
				return err
			}
		case "finish":
			finished, err := finishTourGuide(ctx, out, path, p.VMName)
			if err != nil || finished {
				return err
			}
		default:
			return fmt.Errorf("unknown tour action %q", action)
		}
	}
}

func finishTourGuide(ctx context.Context, out io.Writer, path, name string) (bool, error) {
	action, err := tourGuidePick(ctx, out, "Finish the tour", []tui.PickerItem{
		guideItem("0", "Leave the simulator running", "Keep the robot viewer available", "leave"),
		guideItem("1", "Stop the simulator", "Shut down the VM; your project stays saved", "stop"),
		guideItem("2", "Back", "Keep exploring", "back"),
	})
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(err, errTourGuideBack) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if action == "back" {
		return false, nil
	}
	if action == "stop" {
		if err := runTourStep(ctx, out, "Stopping your simulator", func() error { return tourGuideStop(ctx, name) }); err != nil {
			return false, err
		}
	} else if action != "leave" {
		return false, fmt.Errorf("unknown finish action %q", action)
	}
	guideResume(out, path, name, action == "stop")
	return true, nil
}

func guideResume(out io.Writer, path, name string, stopped bool) {
	if stopped {
		fmt.Fprintln(out, "Your simulator is stopped. Your work is saved.")
	} else {
		fmt.Fprintf(out, "Your simulator is still running. Stop it later with wendy vm stop %s.\n", name)
	}
	fmt.Fprintf(out, "Continue whenever you're ready: %s\n", tourRobotResumeCommand(path))
}

func runTourWalkGuide(ctx context.Context, out io.Writer, p *tourRobotProject, path string, port int, profile vm.RobotProfile) error {
	action, err := tourGuidePick(ctx, out, "Choose a walk to save and try", []tui.PickerItem{
		guideItem("0", "Walk straight", "Keep the same speed and duration", "straight"),
		guideItem("1", "Turn left", "Walk a left-hand arc", "left"),
		guideItem("2", "Turn right", "Walk a right-hand arc", "right"),
		guideItem("3", "Back", "Keep your current walk", "back"),
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errTourGuideBack) {
		return nil
	}
	if err != nil || action == "back" {
		return err
	}
	turn, ok := map[string]float64{"straight": 0, "left": .5, "right": -.5}[action]
	if !ok {
		return fmt.Errorf("unknown walk %q", action)
	}
	if err := runTourStep(ctx, out, "Saving your walk", func() error {
		updated, err := saveTourWalk(path, p.VMName, turn)
		if err == nil {
			*p = updated
		}
		return err
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%s\nWatch the viewer. Your new walk lasts %.0f seconds.\n", guideHeadingStyle.Render("Try your change"), p.DurationSeconds)
	var distance float64
	if err := runTourStep(ctx, out, "Walking your simulated robot", func() error {
		var err error
		distance, err = tourGuideWalk(ctx, port, profile, *p)
		return err
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "Your robot walked %.2f metres and stopped. Saved in %s.\n", distance, path)
	return nil
}

// Reload before changing one setting so other edits made during the tour survive.
func saveTourWalk(path, name string, turn float64) (tourRobotProject, error) {
	before, err := os.ReadFile(path)
	if err != nil {
		return tourRobotProject{}, err
	}
	p, _, err := loadTourRobotProject(filepath.Dir(path))
	if err != nil {
		return p, err
	}
	if p.VMName != name {
		return p, errors.New("the starter now names a different VM; restore its vm_name before continuing this tour")
	}
	p.TurnRPS = turn
	if err := p.validate(); err != nil {
		return p, err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return p, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tour-*.json")
	if err != nil {
		return p, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return p, err
	}
	if err := f.Close(); err != nil {
		return p, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if !bytes.Equal(before, current) {
		return p, errors.New("the starter changed while saving; try again to preserve your latest edits")
	}
	return p, os.Rename(f.Name(), path)
}

func pickTourGuide(ctx context.Context, out io.Writer, title string, items []tui.PickerItem) (string, error) {
	return pickTourGuideWithOptions(ctx, out, title, items)
}

func pickTourGuideWithOptions(ctx context.Context, out io.Writer, title string, items []tui.PickerItem, opts ...tea.ProgramOption) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var model tea.Model = tui.NewPickerWithTitle(title)
	model, _ = model.Update(tui.PickerAddMsg{Items: items})
	model, _ = model.Update(tui.PickerDoneMsg{})
	interrupted := false
	filter := func(_ tea.Model, msg tea.Msg) tea.Msg {
		if _, ok := msg.(tea.InterruptMsg); ok {
			interrupted = true
			return tea.KeyMsg{Type: tea.KeyCtrlC}
		}
		if key, ok := msg.(tea.KeyMsg); ok && key.String() == "ctrl+c" {
			interrupted = true
		}
		return msg
	}
	options := append([]tea.ProgramOption{tea.WithContext(ctx), tea.WithOutput(out), tea.WithFilter(filter)}, opts...)
	final, err := tui.RunProgram(tea.NewProgram(model, options...))
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	picker, ok := final.(tui.PickerModel)
	if !ok {
		return "", fmt.Errorf("unexpected tour picker %T", final)
	}
	if picker.Cancelled() {
		if interrupted {
			return "", ErrUserCancelled
		}
		return "", errTourGuideBack
	}
	if picker.Selected() == nil {
		return "", errors.New("no tour action selected")
	}
	value, ok := picker.Selected().Value.(string)
	if !ok {
		return "", errors.New("invalid tour action")
	}
	return value, nil
}

type tourContinueModel struct {
	label                 string
	done, cancelled, back bool
}

func (m tourContinueModel) Init() tea.Cmd { return nil }
func (m tourContinueModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "enter":
			m.done = true
			return m, tea.Quit
		case "q":
			m.back = true
			return m, tea.Quit
		case "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}
func (m tourContinueModel) View() string {
	if m.done || m.cancelled || m.back {
		return ""
	}
	return guideActionStyle.Render(m.label) + "\n"
}
func waitTourGuide(ctx context.Context, out io.Writer, label string) error {
	final, err := tui.RunProgram(tea.NewProgram(tourContinueModel{label: label}, tea.WithContext(ctx), tea.WithOutput(out), tea.WithFilter(tui.InterruptFilter)))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	m, ok := final.(tourContinueModel)
	if !ok {
		return fmt.Errorf("unexpected tour continuation %T", final)
	}
	if m.cancelled {
		return ErrUserCancelled
	}
	if m.back {
		return errTourGuideBack
	}
	return nil
}

func openTourEditor(ctx context.Context, path string) (bool, error) {
	for _, name := range []string{"code", "cursor"} {
		binary, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, binary, "--goto", path)
		return true, cmd.Run()
	}
	return false, nil
}

func runTourAppGuide(ctx context.Context, out io.Writer, name, projectDir string, apps map[string]string) error {
	fmt.Fprintln(out, "\n"+guideHeadingStyle.Render("Program your Go2"))
	fmt.Fprintln(out, "Python decides what the robot does. ROS 2 carries sensor readings and movement commands; the simulator handles balance.")
	kind, err := tourGuidePick(ctx, out, "What would you like to build?", []tui.PickerItem{
		guideItem("0", "Waypoint patrol", "Follow a route once, then stop", "patrol"),
		guideItem("1", "Obstacle-aware roaming", "Use lidar to stop and turn around obstacles", "roam"),
		guideItem("2", "Your own driving controls", "Customize a browser keyboard controller", "teleop"),
		guideItem("3", "Sensor dashboard", "Explore lidar, position, and other sensor data", "sensors"),
		guideItem("4", "Back", "Return to your robot", "back"),
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errTourGuideBack) {
		return nil
	}
	if err != nil || kind == "back" {
		return err
	}
	if kind != "patrol" && kind != "roam" && kind != "teleop" && kind != "sensors" {
		return fmt.Errorf("unknown robot app %q", kind)
	}
	if apps[kind] == "" {
		if err := runTourStep(ctx, out, "Preparing your robot app", func() error {
			path, err := tourGuideScaffold(projectDir, kind)
			if err == nil {
				apps[kind] = path
			}
			return err
		}); err != nil {
			return err
		}
	}
	return runTourEditableApp(ctx, out, name, kind, apps[kind])
}

func runTourEditableApp(ctx context.Context, out io.Writer, name, kind, appPath string) (runErr error) {
	filename := map[string]string{"patrol": "patrol.py", "roam": "controller.py", "teleop": "index.html", "sensors": "dashboard.py"}[kind]
	editHint := map[string]string{
		"patrol":  "In DEFAULT_WAYPOINTS, try changing (1.0, 1.0) to (1.5, 1.0). Each pair is forward and left, in metres.",
		"roam":    "Change STOP_DISTANCE from 0.85 to 1.0 metres to turn away from obstacles sooner.",
		"teleop":  "Change the Enable controls button label to Take control. Keep its id so the controls still work.",
		"sensors": "Change self.trail = deque(maxlen=600) to maxlen=1200 to keep a longer path in the dashboard.",
	}[kind]
	sourcePath := filepath.Join(appPath, filename)
	fmt.Fprintf(out, "\n%s\n%s\n%s\n", guideHeadingStyle.Render("1. Edit your app"), sourcePath, editHint)
	fmt.Fprintln(out, "Open this file in your editor. Save your changes, then choose Deploy here.")
	deployed, active := false, false
	var deployment *tourAppDeployment
	defer func() {
		if deployment == nil {
			return
		}
		if active {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := tourGuideControlApp(stopCtx, name, kind, deployment.nodeName, "stop")
			cancel()
			if err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("stopping your %s app: %w", kind, err))
			}
		}
		// Deploying can claim ROS control even before Start, or before a failed
		// deployment returns. Stop the captured app and release only its grant.
		releaseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tourGuideCleanup(releaseCtx, deployment); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("releasing your %s app's control: %w", kind, err))
		} else if kind != "sensors" {
			fmt.Fprintln(out, "Your app is stopped. The robot is ready for your next activity.")
		}
	}()
	for {
		deployLabel := "Deploy to this simulator"
		if deployed {
			deployLabel = "Deploy saved changes"
		}
		items := []tui.PickerItem{
			guideItem("0", deployLabel, "Build and run your saved code on the same Go2", "deploy"),
			guideItem("1", "Open in editor", "Open the source file with VS Code or Cursor", "edit"),
		}
		if deployed && (kind == "patrol" || kind == "roam") {
			label := "Start route"
			if kind == "roam" {
				label = "Start roaming"
			}
			items = append(items,
				guideItem("2", label, "Let your app command the simulated Go2", "start"),
				guideItem("3", "Stop movement", "Keep the app available for another run", "stop"))
		} else if deployed {
			items = append(items, guideItem("2", "Open your app", "Try the browser interface you just deployed", "open"))
		}
		items = append(items, guideItem("4", "Back to your robot", "Keep your code for later", "back"))
		preferred := "deploy"
		if deployed {
			preferred = "open"
			if kind == "patrol" || kind == "roam" {
				preferred = "start"
			}
			if active {
				preferred = "stop"
			}
		}
		items = preferTourGuideAction(items, preferred)
		action, err := tourGuidePick(ctx, out, "Edit, deploy, and try your app", items)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errTourGuideBack) {
			return nil
		}
		if err != nil {
			return err
		}
		switch action {
		case "back":
			return nil
		case "edit":
			opened, err := tourGuideEditor(ctx, sourcePath)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				fmt.Fprintf(out, "Could not open the editor: %v\n", err)
			}
			if !opened || err != nil {
				fmt.Fprintf(out, "Open %s in your preferred editor.\n", sourcePath)
			}
			fmt.Fprintln(out, "Save your changes in the editor, then choose Deploy here.")
		case "deploy":
			fmt.Fprintln(out, "\n"+guideHeadingStyle.Render("2. Deploy your saved code"))
			if active {
				if err := runTourStep(ctx, out, "Stopping your app", func() error { _, err := tourGuideControlApp(ctx, name, kind, deployment.nodeName, "stop"); return err }); err != nil {
					return err
				}
				active = false
			}
			if err := runTourStep(ctx, out, "Deploying your robot app", func() error {
				// A retry may follow edits to wendy.json, including its app ID.
				// Release the previous attempt before capturing the next one.
				if deployment != nil {
					if err := tourGuideCleanup(ctx, deployment); err != nil {
						return err
					}
					deployment = nil
				}
				var err error
				// Deploy, control and clean up the same captured configuration.
				// Editor saves take effect on the next deployment attempt.
				deployment, err = tourGuidePrepareDeployment(name, kind, appPath)
				if err != nil {
					return err
				}
				return tourGuideDeploy(ctx, deployment)
			}); err != nil {
				return err
			}
			deployed = true
			fmt.Fprintln(out, guideHeadingStyle.Render("3. Try it, then edit again"))
			if kind == "patrol" || kind == "roam" {
				fmt.Fprintln(out, "Your app is deployed. Choose Start when you're ready to move the robot.")
			} else {
				fmt.Fprintln(out, "Your app is deployed. Choose Open your app to try it in the browser.")
			}
		case "start", "stop":
			if !deployed || (kind != "patrol" && kind != "roam") {
				return errors.New("deploy your movement app before controlling it")
			}
			// A lost start response is not evidence that movement never began.
			// Keep cleanup armed until a stop has been acknowledged.
			if action == "start" {
				active = true
			}
			if err := runTourStep(ctx, out, "Controlling your robot app", func() error { _, err := tourGuideControlApp(ctx, name, kind, deployment.nodeName, action); return err }); err != nil {
				return err
			}
			if action == "stop" {
				active = false
			}
			if action == "start" {
				fmt.Fprintln(out, "Start accepted. Watch your robot in the viewer.")
			} else {
				fmt.Fprintln(out, "Your app has stopped commanding movement.")
			}
		case "open":
			if !deployed {
				return errors.New("deploy your app before opening it")
			}
			var url string
			if err := runTourStep(ctx, out, "Opening your app", func() error {
				var err error
				url, err = tourGuideAppURL(ctx, deployment)
				if err == nil && url == "" {
					return errors.New("your app has no browser interface; restore its HTTP entitlement in wendy.json, then Deploy again")
				}
				return err
			}); err != nil {
				return err
			}
			fmt.Fprintf(out, "Your app: %s\n", url)
			_ = tourGuideBrowser(url)
			if err := tourGuideWait(ctx, out, "Try your app, then return here and press Enter to continue."); err != nil && !errors.Is(err, errTourGuideBack) {
				return err
			}
		default:
			return fmt.Errorf("unknown app action %q", action)
		}
	}
}

func preferTourGuideAction(items []tui.PickerItem, preferred string) []tui.PickerItem {
	for i, item := range items {
		if item.Value != preferred {
			continue
		}
		copy(items[1:i+1], items[:i])
		items[0] = item
		break
	}
	for i := range items {
		items[i].SortKey = fmt.Sprintf("%02d", i)
	}
	return items
}

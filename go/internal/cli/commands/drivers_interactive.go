package commands

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

var pickDriverFn = func(title string, items []tui.PickerItem) (string, error) {
	return pickFromItemsWithColumns(title, items, []tui.PickerColumn{
		{Title: "Name", MinWidth: 16, Required: true, Value: func(i tui.PickerItem) string { return i.Name }},
		{Title: "Status", MinWidth: 12, Required: true, Value: func(i tui.PickerItem) string { return i.Type }},
		{Title: "Version", MinWidth: 10, Value: func(i tui.PickerItem) string { return i.AgentVersion }},
		{Title: "Modules", MinWidth: 12, Value: func(i tui.PickerItem) string { return i.Parameters }},
	})
}

var pickDriverActionFn = pickFromItems

func runDriversInteractive(ctx context.Context, source driverManifestOptions) error {
	target, err := resolveDriversTargetFn(ctx)
	if err != nil {
		return err
	}
	defer target.Close()
	deviceType, dl, err := deviceDriverCoords(ctx, target)
	if err != nil {
		return err
	}
	exts, err := driverExtensionsForChannelFn(ctx, deviceType, dl.GetBaseVersion(), source.nightly, source.pr)
	if err != nil {
		// An interrupt ends the command rather than reading as an outage.
		if len(dl.GetInstalled()) == 0 || ctx.Err() != nil {
			return err
		}
		// A registry outage must not prevent removal of an installed add-on.
		cliLogln("%s", tui.WarningMessage(fmt.Sprintf("Could not list published drivers: %v", err)))
		cliLogln("Installed drivers can still be removed.")
		exts = nil
	}
	return runDriversScreen(ctx, target.Agent.DriverService, dl, exts, source)
}

// driverPickerItems includes installed-only drivers so local, stale and
// unreadable add-ons remain manageable without a published replacement.
func driverPickerItems(dl *agentpbv2.ListDriversResponse, exts []extensionEntry) []tui.PickerItem {
	rows := map[string]tui.PickerItem{}
	for _, d := range dl.GetInstalled() {
		state := "installed"
		if driverStale(d, dl.GetKernelVersion()) {
			state = "stale"
		} else if d.GetLoaded() {
			state = "loaded"
		}
		// Prefer a usable installed copy over one staged for another kernel.
		if prev, ok := rows[d.GetName()]; ok && prev.Type != "stale" {
			continue
		}
		rows[d.GetName()] = tui.PickerItem{
			Name: d.GetName(), Type: state, AgentVersion: d.GetVersion(), Parameters: strings.Join(d.GetModulesLoad(), ", "),
			Value: d.GetName(), Hint: "Select to manage this installed driver.",
		}
	}
	for _, e := range exts {
		if !kernelMatches(e, dl.GetKernelVersion()) {
			continue
		}
		row, installed := rows[e.Name]
		if installed {
			// The row keeps what the device runs; the published build is what
			// Reinstall would apply.
			row.Hint = "Select to reinstall or remove this driver."
			if e.Version != "" {
				row.Hint = fmt.Sprintf("Published %s. %s", e.Version, row.Hint)
			}
		} else {
			row = tui.PickerItem{
				Name: e.Name, Type: "available", AgentVersion: e.Version, Parameters: strings.Join(e.ModulesLoad, ", "),
				Value: e.Name, Hint: "Select to install this driver.",
			}
		}
		rows[e.Name] = row
	}
	items := make([]tui.PickerItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, row)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items
}

func runDriversScreen(ctx context.Context, client agentpbv2.WendyDriverServiceClient, dl *agentpbv2.ListDriversResponse, exts []extensionEntry, source driverManifestOptions) error {
	items := driverPickerItems(dl, exts)
	if len(items) == 0 {
		cliLogln("No driver add-ons installed or published for this kernel.")
		return nil
	}
	name, err := pickDriverFn(fmt.Sprintf("Drivers: %s, kernel %s", source.label(dl.GetBaseVersion()), dl.GetKernelVersion()), items)
	if err != nil {
		return err
	}
	installed := false
	for _, d := range dl.GetInstalled() {
		if d.GetName() == name {
			installed = true
			break
		}
	}
	e, available := selectExtension(exts, name, dl.GetKernelVersion())
	var actions []tui.PickerItem
	if available {
		label := "Install"
		if installed {
			label = "Reinstall"
		}
		actions = append(actions, tui.PickerItem{Name: label, Description: e.Version, Value: "install", SortKey: "1"})
	}
	if installed {
		actions = append(actions, tui.PickerItem{Name: "Remove", Value: "remove", SortKey: "2"})
	}
	actions = append(actions, tui.PickerItem{Name: "Cancel", Value: "cancel", SortKey: "3"})
	action, err := pickDriverActionFn("Manage driver "+name, actions)
	if err != nil {
		return err
	}
	switch action {
	case "install":
		if !available {
			return fmt.Errorf("no compatible driver %q published", name)
		}
		spec, err := publishedDriverSpec(e)
		if err != nil {
			return err
		}
		return installDriver(ctx, client, spec, "")
	case "remove":
		if !installed {
			return fmt.Errorf("driver %q is not installed", name)
		}
		confirmed, err := confirmDriverRemovalFn(fmt.Sprintf("Remove driver %s?", name))
		if errors.Is(err, tui.ErrCancelled) {
			return ErrUserCancelled
		}
		if err != nil {
			return err
		}
		if confirmed {
			return removeDriver(ctx, client, name)
		}
	case "cancel":
	default:
		return fmt.Errorf("unknown driver action %q", action)
	}
	cliNotice("Cancelled.")
	return nil
}

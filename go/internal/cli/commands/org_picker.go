//go:build darwin || linux || windows

package commands

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// listOrgsFromCloud fetches every organization the authenticated user belongs
// to, draining the server-streaming ListOrganizations RPC. Declared as a var
// so unit tests can stub it without a live cloud connection.
var listOrgsFromCloud = listOrgsFromCloudImpl

// orgPageSize is the explicit page size sent to ListOrganizations. The server
// applies a small default (20) when the request omits a limit, which silently
// truncated the org picker to the 20 oldest orgs (WDY: orgs past the first page
// were unreachable — see listOrgsFromCloudImpl).
const orgPageSize = 200

// orgPageCap bounds the pagination loop so a server that keeps returning full
// pages cannot spin forever.
const orgPageCap = 50

func listOrgsFromCloudImpl(ctx context.Context, auth *config.AuthConfig) ([]*cloudpb.Organization, error) {
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	client := cloudpb.NewOrganizationServiceClient(conn)

	// The RPC is paginated: it streams one page per call and honours
	// offset/limit. Keep requesting pages until one comes back short (or empty),
	// otherwise only the first page of orgs ever reaches the picker.
	var orgs []*cloudpb.Organization
	offset := int32(0)
	limit := int32(orgPageSize)
	for page := 0; page < orgPageCap; page++ {
		req := &cloudpb.ListOrganizationsRequest{Offset: &offset, Limit: &limit}
		cloudCtx, err := cloudContext(ctx, auth)
		if err != nil {
			return nil, err
		}
		stream, err := client.ListOrganizations(cloudCtx, req)
		if err != nil {
			return nil, fmt.Errorf("listing organizations: %w", err)
		}

		received := int32(0)
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("receiving organizations: %w", err)
			}
			if org := resp.GetOrganization(); org != nil {
				received++
				orgs = append(orgs, org)
			}
		}
		if received < limit {
			break
		}
		offset += received
	}
	return orgs, nil
}

// authOrgIDs builds a set of org IDs for which the user has a stored auth
// entry whose primary certificate belongs to that org. An org in this set has
// local credentials; one absent was discovered via a shared session.
func authOrgIDs(cfg *config.Config) map[int32]bool {
	ids := make(map[int32]bool, len(cfg.Auth))
	for _, a := range cfg.Auth {
		if len(a.Certificates) > 0 {
			ids[int32(a.Certificates[0].OrganizationID)] = true
		}
	}
	return ids
}

// orgPickerColumns defines the three-column layout: org name, numeric ID, and
// whether local credentials are stored for that org.
var orgPickerColumns = []tui.PickerColumn{
	{
		Title:    "Name",
		MinWidth: 20,
		Required: true,
		Value:    func(item tui.PickerItem) string { return item.Name },
	},
	{
		Title:    "Org. ID",
		MinWidth: 8,
		Value:    func(item tui.PickerItem) string { return item.Description },
	},
	{
		Title:    "Credentials",
		MinWidth: 14,
		Value:    func(item tui.PickerItem) string { return item.Type },
	},
}

// buildOrgPickerItems converts orgs into picker rows sorted and sectioned by
// credential availability. Authenticated orgs (ID ascending) appear first,
// then unauthenticated (ID ascending). Sections are suppressed when all orgs
// belong to the same group.
func buildOrgPickerItems(orgs []*cloudpb.Organization, credIDs map[int32]bool) []tui.PickerItem {
	type entry struct {
		org  *cloudpb.Organization
		cred bool
	}
	entries := make([]entry, 0, len(orgs))
	hasAuth := false
	hasUnauth := false
	for _, org := range orgs {
		cred := credIDs[org.GetId()]
		entries = append(entries, entry{org: org, cred: cred})
		if cred {
			hasAuth = true
		} else {
			hasUnauth = true
		}
	}
	// Sort: authenticated first, then by ID ascending within each group.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].cred != entries[j].cred {
			return entries[i].cred // authenticated first
		}
		return entries[i].org.GetId() < entries[j].org.GetId() // ID ascending
	})

	showSections := hasAuth && hasUnauth

	items := make([]tui.PickerItem, 0, len(entries))
	for _, e := range entries {
		id := e.org.GetId()
		idStr := strconv.Itoa(int(id))
		cred := "✗"
		section := ""
		group := 1
		if e.cred {
			cred = "✓"
			group = 0
			if showSections {
				section = "▸ AUTHENTICATED"
			}
		} else if showSections {
			section = "▸ NOT AUTHENTICATED"
		}
		// SortKey keeps ordering stable if the picker internally re-sorts.
		sortKey := fmt.Sprintf("%d_%010d", group, int(id))
		items = append(items, tui.PickerItem{
			Name:        e.org.GetName(),
			Description: idStr,
			Type:        cred,
			DedupKey:    idStr,
			Value:       idStr,
			Section:     section,
			SortKey:     sortKey,
		})
	}
	return items
}

// newOrgPicker builds the interactive org picker and the rows to feed it. The
// picker construction is separated from running the Bubble Tea program so it can
// be driven directly in unit tests (which have no TTY).
//
// Key bindings:
//
//   - d: mark highlighted org as the persisted default.
//   - x: clear the default.
//   - r: remove stored credentials for the highlighted org (if any).
//   - enter: select the highlighted org and close the picker.
//   - q / esc / ctrl+c: quit without selecting.
//
// When copyOnEnter is true, Enter instead copies the highlighted org's ID to the
// clipboard and keeps the picker open (see PickerModel.OnCopyItem). Only the
// management view ('wendy auth list-orgs') sets this; selection flows leave it
// false so Enter selects and proceeds — otherwise the wizard dead-ends with no
// way to resolve an org (WDY-1840).
func newOrgPicker(orgs []*cloudpb.Organization, cfg *config.Config, copyOnEnter bool) (tui.PickerModel, []tui.PickerItem) {
	credIDs := authOrgIDs(cfg)
	items := buildOrgPickerItems(orgs, credIDs)

	picker := tui.NewPickerWithTitleAndColumns("Select an organization", orgPickerColumns)
	if cfg.DefaultOrgID != 0 {
		picker.DefaultKey = strconv.Itoa(int(cfg.DefaultOrgID))
	}

	picker.OnSetDefault = func(item tui.PickerItem) (string, error) {
		idStr, _ := item.Value.(string)
		n, err := strconv.Atoi(idStr)
		if err != nil {
			return "", fmt.Errorf("invalid org ID")
		}
		c, err := config.Load()
		if err != nil {
			return "", fmt.Errorf("could not save default: %w", err)
		}
		c.DefaultOrgID = int32(n)
		if err := config.Save(c); err != nil {
			return "", err
		}
		if !credIDs[int32(n)] {
			return fmt.Sprintf("Default set to %s. No local credentials — run 'wendy auth login' to authenticate.", item.Name), nil
		}
		return fmt.Sprintf("Default set to %s.", item.Name), nil
	}

	picker.OnUnsetDefault = func() (string, error) {
		c, err := config.Load()
		if err != nil {
			return "", fmt.Errorf("could not clear default: %w", err)
		}
		c.DefaultOrgID = 0
		if err := config.Save(c); err != nil {
			return "", err
		}
		return "Default cleared.", nil
	}

	picker.OnRemoveItem = func(item tui.PickerItem) (string, bool, *tui.PickerItem) {
		idStr, _ := item.Value.(string)
		n, err := strconv.Atoi(idStr)
		if err != nil {
			return "Invalid org ID.", true, nil
		}
		orgID := int32(n)
		if !credIDs[orgID] {
			return fmt.Sprintf("No credentials stored for %s.", item.Name), true, nil
		}
		c, err := config.Load()
		if err != nil {
			return fmt.Sprintf("Could not remove credentials: %v", err), true, nil
		}
		filtered := c.Auth[:0]
		for _, a := range c.Auth {
			if len(a.Certificates) == 0 || int32(a.Certificates[0].OrganizationID) != orgID {
				filtered = append(filtered, a)
			}
		}
		c.Auth = filtered
		if err := config.Save(c); err != nil {
			return fmt.Sprintf("Could not save config: %v", err), true, nil
		}
		delete(credIDs, orgID)
		// Move the row to "Not authenticated" rather than removing it entirely.
		updated := item
		updated.Type = "✗"
		updated.Section = "Not authenticated"
		updated.SortKey = fmt.Sprintf("1_%010d", n)
		return fmt.Sprintf("Credentials removed for %s.", item.Name), false, &updated
	}

	// Copy-on-Enter is opt-in: only the management view ('wendy auth list-orgs')
	// wants Enter to copy an ID and stay open. Selection flows must leave this
	// unset so Enter selects and closes; otherwise the picker can never resolve a
	// selection and the install/enroll wizards dead-end (WDY-1840).
	if copyOnEnter {
		picker.OnCopyItem = func(item tui.PickerItem) string {
			idStr, _ := item.Value.(string)
			_ = clipboardWriter(idStr)
			return fmt.Sprintf("Org ID %s (%s) copied to clipboard.", idStr, item.Name)
		}
	}

	return picker, items
}

// pickOrgInteractive shows the interactive org picker (see newOrgPicker for key
// bindings and the copyOnEnter semantics) and returns the chosen org.
func pickOrgInteractive(orgs []*cloudpb.Organization, cfg *config.Config, copyOnEnter bool) (int32, string, error) {
	picker, items := newOrgPicker(orgs, cfg, copyOnEnter)

	p := tea.NewProgram(picker)
	go func() {
		p.Send(tui.PickerAddMsg{Items: items})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return 0, "", fmt.Errorf("picker: %w", err)
	}
	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return 0, "", ErrUserCancelled
	}
	if pm.Selected() == nil {
		return 0, "", fmt.Errorf("no organization selected")
	}

	idStr := pm.Selected().Value.(string)
	n, err := strconv.Atoi(idStr)
	if err != nil {
		return 0, "", fmt.Errorf("invalid org id %q", idStr)
	}
	return int32(n), pm.Selected().Name, nil
}

// OrgResolution holds the resolved organization.
type OrgResolution struct {
	ID   int32
	Name string
}

// resolveOrg determines which organization to use for an operation that must
// target a specific org. It implements the four selection scenarios:
//
//  1. No default + one org    -> use the sole org (no picker).
//  2. No default + many orgs  -> show the picker; the user may set a new default.
//  3. Default set             -> use the default (no picker).
//  4. alwaysPickOrg == true   -> always show the picker regardless of a default;
//     the user may change or clear the default.
//
// If fetching the org list fails, resolveOrg falls back to the org embedded in
// the auth session's certificate and logs a warning.
func resolveOrg(ctx context.Context, auth *config.AuthConfig, alwaysPickOrg bool) (OrgResolution, error) {
	return resolveOrgFn(ctx, auth, alwaysPickOrg)
}

var resolveOrgFn = resolveOrgImpl

func resolveOrgImpl(ctx context.Context, auth *config.AuthConfig, alwaysPickOrg bool) (OrgResolution, error) {
	cfg, err := config.Load()
	if err != nil {
		return OrgResolution{}, fmt.Errorf("loading config: %w", err)
	}
	return resolveOrgWithConfig(ctx, cfg, auth, alwaysPickOrg)
}

// resolveOrgWithConfig is the inner implementation that accepts an already-
// loaded config, making it directly testable without touching the config file.
func resolveOrgWithConfig(ctx context.Context, cfg *config.Config, auth *config.AuthConfig, alwaysPickOrg bool) (OrgResolution, error) {
	orgs, listErr := listOrgsFromCloud(ctx, auth)

	if listErr != nil {
		if alwaysPickOrg {
			return OrgResolution{}, fmt.Errorf("fetching organizations: %w", listErr)
		}
		fmt.Printf("Warning: could not fetch organizations (%v). Falling back to certificate org.\n", listErr)
		certOrgID := int32(0)
		if len(auth.Certificates) > 0 {
			certOrgID = int32(auth.Certificates[0].OrganizationID)
		}
		return OrgResolution{ID: certOrgID, Name: fmt.Sprintf("org %d", certOrgID)}, nil
	}

	if len(orgs) == 0 {
		return OrgResolution{}, fmt.Errorf("your account belongs to no organizations; contact your administrator")
	}

	// Scenario 1: single org — use it without prompting.
	if len(orgs) == 1 && !alwaysPickOrg {
		return OrgResolution{ID: orgs[0].GetId(), Name: orgs[0].GetName()}, nil
	}

	// Scenario 3: valid default is set and picker not forced.
	if cfg.DefaultOrgID != 0 && !alwaysPickOrg {
		for _, org := range orgs {
			if org.GetId() == cfg.DefaultOrgID {
				return OrgResolution{ID: org.GetId(), Name: org.GetName()}, nil
			}
		}
		// Default no longer valid (org removed from membership); fall through to picker.
	}

	// Scenarios 2 and 4 need the interactive picker, which opens /dev/tty. In
	// non-interactive mode (--json, or output is not a terminal) that would
	// crash with "could not open a new TTY"; fail with actionable guidance
	// instead so scripts and `--json` callers get a usable error.
	if !canPromptForOrgFn() {
		return OrgResolution{}, nonInteractiveOrgErr(orgs, cfg.DefaultOrgID)
	}

	// Scenarios 2 and 4: show the interactive picker. copyOnEnter is false so
	// Enter selects an org and lets the wizard proceed (WDY-1840).
	id, name, err := pickOrgInteractiveFn(orgs, cfg, false)
	if err != nil {
		return OrgResolution{}, err
	}
	return OrgResolution{ID: id, Name: name}, nil
}

// pickOrgInteractiveFn is stubbed in tests.
var pickOrgInteractiveFn = pickOrgInteractive

// canPromptForOrgFn reports whether an interactive org picker can be shown.
// It is false under --json or when output is not a terminal (jsonOutput is set
// from both; see root.go). Overridable in tests.
var canPromptForOrgFn = func() bool { return !jsonOutput }

// nonInteractiveOrgErr explains why an org could not be resolved without a
// prompt and how to fix it, listing the available organizations.
func nonInteractiveOrgErr(orgs []*cloudpb.Organization, defaultOrgID int32) error {
	avail := ""
	for i, o := range orgs {
		if i > 0 {
			avail += ", "
		}
		avail += fmt.Sprintf("%d (%s)", o.GetId(), o.GetName())
	}
	if defaultOrgID != 0 {
		return fmt.Errorf("default organization %d is not one of your organizations [%s]; pass --org <id> (where supported) or set a valid default with 'wendy auth list-orgs' in an interactive terminal", defaultOrgID, avail)
	}
	return fmt.Errorf("cannot select an organization non-interactively: your account belongs to multiple organizations [%s]; pass --org <id> (where supported) or set a default with 'wendy auth list-orgs' in an interactive terminal", avail)
}

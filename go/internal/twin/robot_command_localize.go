package twin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

type passiveRobotLocalizer interface {
	LocalizeCommand(context.Context, twinrobot.SiteFusionRequest) twinrobot.CommandResult
}

func (s *Server) localizationRoom(name string) (Room, error) {
	if s.Store == nil {
		return Room{}, errors.New("the digital twin has no room store")
	}
	rooms, err := s.Store.List()
	if err != nil {
		return Room{}, err
	}
	name = strings.TrimSpace(name)
	var candidates []Room
	for _, room := range rooms {
		if room.Removed {
			continue
		}
		if name != "" {
			if strings.EqualFold(strings.TrimSpace(room.Site.Name), name) {
				candidates = append(candidates, room)
			}
		} else if room.HasLocalizationGeometry {
			candidates = append(candidates, room)
		}
	}
	if len(candidates) == 0 {
		if name != "" {
			return Room{}, fmt.Errorf("no saved room named %q; choose an existing digital-twin room", name)
		}
		return Room{}, errors.New("no saved room has localization geometry; open a saved room's localization view once to upload its architectural geometry")
	}
	if len(candidates) > 1 {
		names := make([]string, 0, len(candidates))
		for _, room := range candidates {
			names = append(names, room.Site.Name)
		}
		return Room{}, fmt.Errorf("more than one saved room matches; specify one exact room name: %s", strings.Join(names, ", "))
	}
	room, err := s.Store.Frame(candidates[0].Site.ID + "/" + candidates[0].Site.ModelRevision)
	if err != nil {
		return Room{}, err
	}
	if room.LocalizationGeometry == nil || !room.HasLocalizationGeometry || room.LocalizationGeometry.Frame != room.Site.ID+"/"+room.Site.ModelRevision {
		return Room{}, fmt.Errorf("room %q has no localization geometry for its current revision; open its localization view to upload the saved architectural geometry", room.Site.Name)
	}
	return room, nil
}

func (s *Server) localizeCommand(ctx context.Context, session *robotSession, q robotCommandRequest, controller robotCommander) twinrobot.CommandResult {
	reject := func(message string) twinrobot.CommandResult {
		return twinrobot.CommandResult{State: "rejected", Message: message, Snapshot: session.robot.Status()}
	}
	if ctx.Err() != nil {
		return twinrobot.CommandResult{State: "canceled", Message: ctx.Err().Error(), Snapshot: session.robot.Status()}
	}
	state := session.robot.Status()
	if state.Moving || controller.CommandActive() {
		return reject("A movement command is active. Wait for its outcome or stop it before localizing.")
	}
	// A motion command can be waiting for startup measurements before acquiring
	// the engine's active intent. Do not alter its coordinate frame in that gap.
	s.mu.Lock()
	pendingSetup := len(s.commandSetups[session.serial]) > 1
	s.mu.Unlock()
	session.commandMu.Lock()
	pendingCommand := false
	for id, receipt := range session.commandReceipts {
		if id != q.RequestID && receipt.result.State == "running" {
			pendingCommand = true
			break
		}
	}
	session.commandMu.Unlock()
	if pendingSetup || pendingCommand {
		return reject("Another robot command is pending. Resolve or stop it before localizing.")
	}
	localizer, ok := session.robot.(passiveRobotLocalizer)
	if !ok {
		return reject("This digital-twin controller needs the passive localization update.")
	}
	room, err := s.localizationRoom(q.Destination)
	if err != nil {
		return reject(err.Error())
	}
	geometry := room.LocalizationGeometry
	input := twinrobot.SiteFusionRequest{Frame: geometry.Frame, Architecture: geometry.Architecture, Furniture: geometry.Furniture, Reference: room.References[session.serial]}
	var result twinrobot.CommandResult
	// Bind matching and confirmation to this persisted geometry revision. A
	// concurrent room removal cannot make a confirmed result use a stale frame.
	err = s.Store.withFrame(input.Frame, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		result = localizer.LocalizeCommand(ctx, input)
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return twinrobot.CommandResult{State: "canceled", Message: ctx.Err().Error(), Snapshot: session.robot.Status()}
		}
		return reject(err.Error())
	}
	if result.State == "completed" && (!result.Snapshot.Aligned || result.Snapshot.Frame != input.Frame) {
		return reject("The controller did not confirm a position in the selected room.")
	}
	return result
}

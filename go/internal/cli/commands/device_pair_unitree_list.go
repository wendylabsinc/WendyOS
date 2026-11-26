package commands

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func runUnitreeList(ctx context.Context, client pb.WendyUnitreeServiceClient, jsonOut bool, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	resp, err := client.ListUnitreePairings(ctx, &pb.ListUnitreePairingsRequest{})
	if err != nil {
		return cleanRPCError(err)
	}
	if jsonOut {
		return writePairProtoJSON(out, resp)
	}
	for _, robot := range resp.GetRobots() {
		state := "disconnected"
		if robot.Connected {
			state = "connected"
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", tui.StripControl(robot.Serial), tui.StripControl(robot.Name), tui.StripControl(robot.Address), state); err != nil {
			return err
		}
	}
	if len(resp.GetRobots()) == 0 {
		_, err = fmt.Fprintln(out, "No saved Unitree pairings.")
	}
	return err
}

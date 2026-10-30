package liteclient

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/shared/litetopology"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	lite "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (c *WendyLiteClient) NetTopology(parent context.Context) (*pb.LocalMeshTopology, error) {
	ctx, cancel := context.WithTimeout(parent, litetopology.AggregateBudget)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	return litetopology.Read(ctx, func(ctx context.Context, params *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		// sendCommand starts its response timer after writing. Bound the private
		// stream too so a backpressured write cannot consume the whole read budget.
		ctx, cancel := context.WithTimeout(ctx, litetopology.PageBudget)
		defer cancel()
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		defer stop()
		response, err := c.sendCommand(&lite.WendyComCommand{RequestId: c.requestIdGen.Add(1), Params: &lite.WendyComCommand_NetTopology{NetTopology: params}}, litetopology.PageBudget)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		if response.Result == lite.WendyComResult_WENDY_COM_RESULT_BUSY {
			return nil, status.Error(codes.Aborted, "topology view changed or is busy")
		}
		if response.Result != lite.WendyComResult_WENDY_COM_RESULT_OK {
			return nil, status.Error(codes.Unimplemented, "topology is unsupported or not ready")
		}
		return response.GetNetTopology(), nil
	})
}

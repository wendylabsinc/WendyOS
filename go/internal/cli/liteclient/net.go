package liteclient

import (
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func (c *WendyLiteClient) GetNetStatus(timeout time.Duration) (*litepb.WendyComNetStatus, error) {
	return c.netCommand(&litepb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params:    &litepb.WendyComCommand_NetGet{NetGet: &litepb.WendyComNetGetParams{}},
	}, timeout)
}

func (c *WendyLiteClient) SetNetConfig(config *litepb.WendyComNetConfig, timeout time.Duration) (*litepb.WendyComNetStatus, error) {
	return c.netCommand(&litepb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params:    &litepb.WendyComCommand_NetSet{NetSet: &litepb.WendyComNetSetParams{Config: config}},
	}, timeout)
}

func (c *WendyLiteClient) netCommand(command *litepb.WendyComCommand, timeout time.Duration) (*litepb.WendyComNetStatus, error) {
	response, err := c.sendCommand(command, timeout)
	if err != nil {
		return nil, err
	}
	status := response.GetNetStatus()
	if response.Result == litepb.WendyComResult_WENDY_COM_RESULT_APPLIED_RESTARTING {
		if command.GetNetSet() == nil || status == nil {
			return nil, fmt.Errorf("invalid restarting response to Wendy Net command")
		}
		status.Restarting = true
		return status, nil
	}
	if err := resultToError(response.Result); err != nil {
		if status != nil {
			return status, fmt.Errorf("Wendy Net settings: %w (device code %d)", err, status.GetErrorCode())
		}
		return nil, fmt.Errorf("Wendy Net settings unavailable: %w", err)
	}
	if status == nil {
		return nil, fmt.Errorf("device returned no Wendy Net status")
	}
	return status, nil
}

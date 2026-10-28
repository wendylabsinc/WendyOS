package localmesh

import (
	"errors"
	"log"
)

// A control owner may deliberately end a link before its stream readers fail.
// Keep that first local cause instead of replacing it with net.ErrClosed. The
// hub callback can run outside the Node actor, so use only immutable peer fields
// and concurrency-safe channel lengths here. No packet or artifact is logged.
func (n *Node) closePeer(p *nodePeer, stage string, cause error) {
	p.closeDiagnostic.Do(func() {
		reason := boundedCarrierCloseReason(stage, cause)
		log.Printf("wendy net local carrier close peer=%q link=%d transport=%q control_queued=%d reason=%q",
			p.device.Principal(), p.id, diagnosticText(p.transport, 32), len(p.control), reason)
		if recorder, ok := p.link.(interface{ recordTerminalError(error) }); ok {
			recorder.recordTerminalError(errors.New(reason))
		}
		_ = p.link.Close()
	})
}

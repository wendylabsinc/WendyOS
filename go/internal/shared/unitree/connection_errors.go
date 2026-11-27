package unitree

import (
	"context"
	"errors"
)

// ConnectionFailureMessage exposes only locally defined failure descriptions.
// Pion and peer-controlled SDP errors must never be echoed to clients or logs.
func ConnectionFailureMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "Go2 connection setup timed out. Check the robot's IP address and network. If another operator is connected, arrange a handoff before retrying."
	}
	if errors.Is(err, context.Canceled) {
		return "Go2 connection was canceled."
	}
	if err == nil {
		return ""
	}
	if message, ok := connectionFailureMessages[err.Error()]; ok {
		return message
	}
	return connectionFailureFallback
}

// SafeConnectionFailureMessage accepts only complete, locally defined diagnostic
// messages from an agent. Never display arbitrary RPC text containing robot keys
// or peer-controlled responses, including text appended to a known message.
func SafeConnectionFailureMessage(message string) string {
	// Older agents use the original busy diagnostic. Keep its exact match
	// compatible while giving users the same actionable guidance.
	if message == "Go2 is already connected to another controller. Arrange a handoff with its operator before retrying." {
		return connectionBusyMessage
	}
	for _, allowed := range connectionFailureMessages {
		if message == allowed {
			return allowed
		}
	}
	for _, err := range []error{context.DeadlineExceeded, context.Canceled} {
		if allowed := ConnectionFailureMessage(err); message == allowed {
			return allowed
		}
	}
	return connectionFailureFallback
}

const connectionBusyMessage = "Go2 is already connected to another controller. Disconnect the Unitree Go app or ask the other operator to disconnect, then retry."

const connectionFailureFallback = "Go2 connection setup failed before the robot was ready. Check its IP address and network."

var connectionFailureMessages = map[string]string{
	"Unitree signaling is unreachable on ports 9991 and 8081; check the IP address and LAN connection": "The robot's connection service is unreachable on ports 9991 and 8081. Check its IP address and that the device connecting to it can reach the robot's network.",
	"keyed Unitree robots require authenticated encrypted signaling; legacy fallback is refused":       "The robot's encrypted connection service is unavailable on port 9991. Check its IP address and network. Wendy will not send the robot key over a legacy connection.",
	"Unitree signaling request failed; check the robot's LAN connection":                               "The robot did not answer the connection request. Check its IP address and network.",
	"Unitree is busy; arrange an explicit handoff with the current robot operator before retrying":     connectionBusyMessage,
	"this Unitree firmware requires its encryption key; sign in to the robot's Unitree account":        "This Go2 requires a robot encryption key. Use the Unitree Go account bound to this robot.",
	"Unitree rejected the encryption key; select the account robot that matches this IP address":       "Go2 rejected the robot encryption key. Check that the serial number and IP address refer to the same robot.",
	"keyed Unitree robots require device-key-authenticated signaling":                                  "The robot did not provide the encrypted handshake required for its account key. Check that the serial number and IP address refer to the same robot.",
	"Unitree WebRTC connection closed before validation":                                               "Go2 disconnected before setup finished. Check the network and whether another controller is connected.",
	"Unitree data channel closed before validation":                                                    "Go2 disconnected before setup finished. Check the network and whether another controller is connected.",
	"Unitree disconnected during validation":                                                           "Go2 disconnected before setup finished. Check the network and whether another controller is connected.",
	"Unitree returned an invalid SDP answer":                                                           "Go2 returned an invalid connection response. Check that the IP address belongs to the robot.",
	"Unitree returned an unsupported SDP answer":                                                       "Go2 returned an invalid connection response. Check that the IP address belongs to the robot.",
	"Unitree returned an unexpected SDP message type":                                                  "Go2 returned an invalid connection response. Check that the IP address belongs to the robot.",
	"Unitree returned an invalid encrypted SDP answer":                                                 "Go2 returned an invalid connection response. Check that the IP address belongs to the robot.",
	"Unitree uses an unsupported signaling encryption version":                                         "This Go2 firmware uses an unsupported connection protocol.",
}

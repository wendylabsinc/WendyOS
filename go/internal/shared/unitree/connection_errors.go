package unitree

import (
	"context"
	"errors"
)

// ConnectionFailureMessage exposes only locally defined failure descriptions.
// Pion and peer-controlled SDP errors must never be echoed into the login form.
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
	switch err.Error() {
	case "Unitree signaling is unreachable on ports 9991 and 8081; check the IP address and LAN connection":
		return "The robot's connection service is unreachable on ports 9991 and 8081. Check its IP address and that this Mac can reach the robot's network."
	case "keyed Unitree robots require authenticated encrypted signaling; legacy fallback is refused":
		return "The robot's encrypted connection service is unavailable on port 9991. Check its IP address and network. Wendy will not send the robot key over a legacy connection."
	case "Unitree signaling request failed; check the robot's LAN connection":
		return "The robot did not answer the connection request. Check its IP address and network."
	case "Unitree is busy; arrange an explicit handoff with the current robot operator before retrying":
		return "Go2 is already connected to another controller. Arrange a handoff with its operator before retrying."
	case "this Unitree firmware requires its encryption key; sign in to the robot's Unitree account":
		return "This Go2 requires a robot encryption key. Use the Unitree Go account bound to this robot."
	case "Unitree rejected the encryption key; select the account robot that matches this IP address":
		return "Go2 rejected the robot encryption key. Check that the serial number and IP address refer to the same robot."
	case "keyed Unitree robots require device-key-authenticated signaling":
		return "The robot did not provide the encrypted handshake required for its account key. Check that the serial number and IP address refer to the same robot."
	case "Unitree WebRTC connection closed before validation", "Unitree data channel closed before validation", "Unitree disconnected during validation":
		return "Go2 disconnected before setup finished. Check the network and whether another controller is connected."
	case "Unitree returned an invalid SDP answer", "Unitree returned an unsupported SDP answer", "Unitree returned an unexpected SDP message type", "Unitree returned an invalid encrypted SDP answer":
		return "Go2 returned an invalid connection response. Check that the IP address belongs to the robot."
	case "Unitree uses an unsupported signaling encryption version":
		return "This Go2 firmware uses an unsupported connection protocol."
	default:
		return "Go2 connection setup failed before the robot was ready. Check its IP address and network."
	}
}

package network

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// ConnectFailure is the reason a WiFi connect failed.
type ConnectFailure int

const (
	// ConnectFailureUnknown is a failure nmcli worded in a way not known here.
	ConnectFailureUnknown ConnectFailure = iota
	// ConnectFailureWrongPassword is a network rejecting the password given.
	ConnectFailureWrongPassword
	// ConnectFailurePasswordRequired is a secured network joined without one.
	ConnectFailurePasswordRequired
	// ConnectFailureNetworkNotFound is a network the device does not see.
	ConnectFailureNetworkNotFound
	// ConnectFailureInvalidPassword is a password of a length WPA does not take.
	ConnectFailureInvalidPassword
	// ConnectFailureTimedOut is a network that did not answer in time.
	ConnectFailureTimedOut
	// ConnectFailureNoAddress is a joined network that handed out no address.
	ConnectFailureNoAddress
	// ConnectFailureNoAdapter is a device without a WiFi adapter.
	ConnectFailureNoAdapter
)

// ConnectError is a failed WiFi connect. Its message is for the person who
// asked for the connect and reads on from "failed to connect: ". Err is the
// error as the connect returned it, with nmcli's own words.
type ConnectError struct {
	Reason ConnectFailure
	SSID   string
	Err    error
}

func (e *ConnectError) Error() string {
	switch e.Reason {
	case ConnectFailureWrongPassword:
		return fmt.Sprintf("%q did not accept the password", e.SSID)
	case ConnectFailurePasswordRequired:
		return fmt.Sprintf("%q needs a password", e.SSID)
	case ConnectFailureNetworkNotFound:
		return fmt.Sprintf("no Wi-Fi network named %q is in range of the device", e.SSID)
	case ConnectFailureInvalidPassword:
		return "a Wi-Fi password is 8 to 63 characters"
	case ConnectFailureTimedOut:
		return fmt.Sprintf("%q did not answer in time; the device may be too far from it, or the password may be wrong", e.SSID)
	case ConnectFailureNoAddress:
		return fmt.Sprintf("joined %q but it gave the device no IP address", e.SSID)
	case ConnectFailureNoAdapter:
		return "the device has no Wi-Fi adapter"
	}
	if e.Err != nil {
		if words := nmcliWords(e.Err.Error(), e.SSID); words != "" {
			return words
		}
	}
	return fmt.Sprintf("the device could not join %q", e.SSID)
}

func (e *ConnectError) Unwrap() error { return e.Err }

// ConnectToWiFi joins the network in req. A failure to join it is a
// *ConnectError; what nmcli said about it goes to the device log.
func (n *NMCLINetworkManager) ConnectToWiFi(ctx context.Context, req *agentpb.ConnectToWiFiRequest) error {
	err := n.connectToWiFi(ctx, req)
	if err == nil {
		return nil
	}
	n.logger.Warn("WiFi connect failed", zap.String("ssid", req.GetSsid()), zap.Error(err))
	return connectError(ctx, req, err)
}

// profileCheckPrefix starts the error for saved profiles that can't be read.
const profileCheckPrefix = "checking for existing WiFi profile: "

// connectError words a failed connect for the person who asked for it. A
// cancelled request, an unreachable NetworkManager and unreadable saved
// profiles say nothing about the network, so those errors stay as they are.
func connectError(ctx context.Context, req *agentpb.ConnectToWiFiRequest, err error) error {
	if ctx.Err() != nil || NetworkManagerUnavailable(err) || strings.HasPrefix(err.Error(), profileCheckPrefix) {
		return err
	}
	ssid := req.GetSsid()
	reason := connectFailure(nmcliWords(err.Error(), ssid), req.GetPassword() != "")
	return &ConnectError{Reason: reason, SSID: ssid, Err: err}
}

// connectFailure reads the reason for a failed connect from nmcli's words.
// nmcli words a rejected password and a missing one alike, so hadPassword
// tells them apart.
func connectFailure(words string, hadPassword bool) ConnectFailure {
	words = strings.ToLower(words)
	has := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(words, part) {
				return true
			}
		}
		return false
	}
	switch {
	case has("802-11-wireless-security.psk") && has("invalid"):
		return ConnectFailureInvalidPassword
	case has("secrets were required"):
		if hadPassword {
			return ConnectFailureWrongPassword
		}
		return ConnectFailurePasswordRequired
	case has("no network with ssid", "network could not be found"):
		return ConnectFailureNetworkNotFound
	// Checked before the timeouts, since nmcli's words for it mention one.
	case has("ip configuration could not be reserved"):
		return ConnectFailureNoAddress
	case has("timeout", "timed out", "took too long"):
		return ConnectFailureTimedOut
	case has("no wi-fi device", "no wifi device"):
		return ConnectFailureNoAdapter
	}
	return ConnectFailureUnknown
}

// exitStatusRe matches the exit status that ends the error of a failed command.
var exitStatusRe = regexp.MustCompile(`: exit status \d+$`)

// nmcliWords returns what nmcli printed from the text of a connect error:
// without the command the connect helpers name in front of it, nmcli's
// "Error: " and the exit status.
func nmcliWords(text, ssid string) string {
	text = strings.TrimPrefix(text, "preparing WiFi profile: ")
	for _, command := range []string{"connect", "connection up " + ssid, "connection add " + ssid, "connection modify " + ssid} {
		text = strings.TrimPrefix(text, "nmcli "+command+": ")
	}
	text = exitStatusRe.ReplaceAllString(text, "")
	// nmcli printed nothing when the text starts at the separator.
	text = strings.TrimSpace(strings.TrimPrefix(text, ": "))
	return strings.TrimPrefix(text, "Error: ")
}

package meshsession

import (
	"errors"
	"net"
)

var errLocalUDPPlatform = errors.New("local Wendy Net UDP app delivery requires Unix")

func configureLocalUDP(*net.UDPConn) error      { return errLocalUDPPlatform }
func writeLocalUDP(*net.UDPConn, []byte) error  { return errLocalUDPPlatform }
func localUDPBuffers(*net.UDPConn) (int, error) { return 0, errLocalUDPPlatform }
func udpReadTruncated(int) bool                 { return true }

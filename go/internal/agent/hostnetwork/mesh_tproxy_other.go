//go:build !linux

package hostnetwork

import "errors"

var errMeshUDPPlatform = errors.New("mesh UDP VIP interception requires Linux")

func InitMeshUDPTProxy(int) error                              { return errMeshUDPPlatform }
func EnsureMeshUDPTProxyPolicy() error                         { return errMeshUDPPlatform }
func CloseMeshUDPTProxy() error                                { return nil }
func AddMeshUDPIntercept(string, string, string, int) error    { return errMeshUDPPlatform }
func RemoveMeshUDPIntercept(string, string, string, int) error { return nil }

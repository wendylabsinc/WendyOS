//go:build !linux

package appservice

import "net"

// Production attribution is Linux-only. Unsupported kernels never fall back
// to UID, mount visibility, headers, or an app-wide credential.
func readPeerCredentials(*net.UnixConn) (Peer, error) { return Peer{}, ErrPeer }
func pinOriginalPeer(*net.UnixConn) (peerPin, error)  { return nil, ErrPeer }

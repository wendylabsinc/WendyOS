//go:build !linux

package rtps

import "errors"

type networkNamespace struct {
	identity    namespaceIdentity
	host        bool
	verifyOwner func() bool
}

func captureNetworkNamespace(pid uint32, verify func() bool) (*networkNamespace, error) {
	if pid != 0 {
		return nil, errors.New("rtps: network namespaces require Linux")
	}
	n := &networkNamespace{identity: namespaceIdentity{inode: 1}, host: true, verifyOwner: verify}
	return n, n.verify()
}
func (n *networkNamespace) close()        {}
func (n *networkNamespace) verify() error { return verifyNamespaceTarget(0, n.verifyOwner) }
func (n *networkNamespace) run(fn func() error) error {
	if err := n.verify(); err != nil {
		return err
	}
	return fn()
}
func withNetworkNamespace(pid uint32, verify func() bool, fn func() error) error {
	n, err := captureNetworkNamespace(pid, verify)
	if err != nil {
		return err
	}
	return n.run(fn)
}

//go:build windows

package useripc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func currentSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}
func privateDescriptor(inherit bool) (string, error) {
	sid, err := currentSID()
	if err != nil {
		return "", err
	}
	flags := ""
	if inherit {
		flags = "OICI"
	}
	return "O:" + sid.String() + "D:P(A;" + flags + ";GA;;;" + sid.String() + ")(A;" + flags + ";GA;;;SY)", nil
}
func ownedDescriptor(sd *windows.SECURITY_DESCRIPTOR) error {
	if sd == nil {
		return errors.New("session broker object has no security descriptor")
	}
	sid, err := currentSID()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(sid) {
		return errors.New("session broker path is not owned by the current user")
	}
	return nil
}
func privateDescriptorValid(sd *windows.SECURITY_DESCRIPTOR) error {
	if err := ownedDescriptor(sd); err != nil {
		return err
	}
	sid, err := currentSID()
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if acl == nil || acl.AceCount == 0 {
		return errors.New("session broker object has no private ACL")
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("unsupported session broker ACL entry")
		}
		trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !trustee.Equals(sid) && !trustee.IsWellKnown(windows.WinLocalSystemSid) {
			return errors.New("session broker ACL grants another account access")
		}
	}
	return nil
}
func pathDescriptor(path string) (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
}
func OwnedPath(path string) bool {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	sd, err := pathDescriptor(path)
	return err == nil && ownedDescriptor(sd) == nil
}
func EnsureDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	sd, err := pathDescriptor(path)
	if err != nil {
		return err
	}
	if err = ownedDescriptor(sd); err != nil {
		return err
	}
	text, err := privateDescriptor(true)
	if err != nil {
		return err
	}
	private, err := windows.SecurityDescriptorFromString(text)
	if err != nil {
		return err
	}
	acl, _, err := private.DACL()
	if err != nil {
		return err
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return err
	}
	return ValidateDirectory(path)
}
func ValidateDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("session directory is not a directory")
	}
	sd, err := pathDescriptor(path)
	if err != nil {
		return err
	}
	return privateDescriptorValid(sd)
}
func ValidateFile(path string, socket bool) error {
	// Named pipes have no filesystem inode. Their owner and ACL are checked on
	// the connected handle in Dial, before any RPC bytes are written.
	if socket {
		return ValidateDirectory(filepath.Dir(path))
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("session broker state is not a regular file")
	}
	sd, err := pathDescriptor(path)
	if err != nil {
		return err
	}
	return privateDescriptorValid(sd)
}
func pipeName(path string) (string, error) {
	sid, err := currentSID()
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sid.String() + "\x00" + strings.ToLower(filepath.Clean(absolute))))
	return `\\.\pipe\wendy-session-` + hex.EncodeToString(sum[:]), nil
}
func Listen(path string) (net.Listener, error) {
	name, err := pipeName(path)
	if err != nil {
		return nil, err
	}
	descriptor, err := privateDescriptor(false)
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: descriptor, InputBufferSize: 16 * 1024, OutputBufferSize: 16 * 1024})
}
func Dial(ctx context.Context, path string) (net.Conn, error) {
	name, err := pipeName(path)
	if err != nil {
		return nil, err
	}
	connection, err := winio.DialPipeAccess(ctx, name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL)
	if err != nil {
		return nil, err
	}
	file, ok := connection.(interface{ Fd() uintptr })
	if !ok {
		connection.Close()
		return nil, fmt.Errorf("named pipe has no inspectable handle")
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err == nil {
		err = privateDescriptorValid(descriptor)
	}
	if err != nil {
		connection.Close()
		return nil, err
	}
	return connection, nil
}

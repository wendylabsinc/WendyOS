package operator

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"time"
)

const keychainTimeout = 5 * time.Second

type securityRunner func(context.Context, ...string) ([]byte, error)
type commandKeychain struct{ run securityRunner }

// SystemKeychain is the existing macOS generic-password read protocol. Other
// platforms return nil and must use their existing inline configuration custody.
func SystemKeychain() Keychain {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return commandKeychain{run: runSecurity}
}
func (k commandKeychain) Get(ctx context.Context, account string) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || k.run == nil || !keyAccount.MatchString(account) {
		return nil, errors.New("keychain lookup unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, keychainTimeout)
	defer cancel()
	out, err := k.run(ctx, "find-generic-password", "-s", keychainService, "-a", account, "-w")
	defer clear(out)
	if err != nil || ctx.Err() != nil || len(out) > base64.StdEncoding.EncodedLen(MaxKeyPEMBytes)+128 {
		return nil, errors.New("keychain read failed")
	}
	value := bytes.TrimSpace(out)
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(value)))
	n, err := base64.StdEncoding.Strict().Decode(decoded, value)
	if err != nil || n == 0 || n > MaxKeyPEMBytes {
		clear(decoded)
		return nil, errors.New("keychain value has unsupported encoding or size")
	}
	return decoded[:n], nil
}

type boundedOutput struct {
	bytes.Buffer
	max      int
	exceeded bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		b.exceeded = true
		return 0, errors.New("subprocess output exceeds bound")
	}
	return b.Buffer.Write(p)
}
func runSecurity(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	out := &boundedOutput{max: base64.StdEncoding.EncodedLen(MaxKeyPEMBytes) + 128}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if err != nil || out.exceeded {
		clear(out.Bytes())
		return nil, errors.New("keychain read unavailable")
	}
	return out.Bytes(), nil
}

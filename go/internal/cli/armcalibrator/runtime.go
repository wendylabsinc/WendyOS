// Package armcalibrator bundles the standalone YAM calibration wizard.
// The runtime uses Python's standard library; no device-side install is needed.
package armcalibrator

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"os/exec"
	"time"
)

//go:embed runtime.pyz
var runtime []byte

// Bootstrap runs the same verified archive locally and through HostShell.
// Arguments remain argv throughout: no shell interpolation or pip install.
// A temporary directory is removed when the operator finishes or interrupts.
const bootstrap = `import base64, hashlib, pathlib, signal, subprocess, sys, tempfile
if sys.version_info < (3, 10):
    sys.exit("Arm calibration needs Python 3.10 or newer")
data = base64.b64decode(sys.argv[1], validate=True)
if hashlib.sha256(data).hexdigest() != sys.argv[2]:
    sys.exit("Calibration runtime checksum mismatch")
def interrupt(signum, frame):
    raise KeyboardInterrupt
signal.signal(signal.SIGTERM, interrupt)
if hasattr(signal, "SIGHUP"):
    signal.signal(signal.SIGHUP, interrupt)
with tempfile.TemporaryDirectory(prefix="wendy-arm-calibrator-") as directory:
    path = pathlib.Path(directory) / "runtime.pyz"
    path.write_bytes(data)
    child = subprocess.Popen([sys.executable, str(path)] + sys.argv[3:])
    try:
        result = child.wait()
    except KeyboardInterrupt:
        child.terminate()
        try:
            child.wait(timeout=5)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait()
        result = 130
sys.exit(result)
`

// Command returns an argv ready for exec or the authenticated device HostShell.
func Command(args []string) []string {
	digest := sha256.Sum256(runtime)
	command := []string{"python3", "-c", bootstrap, base64.StdEncoding.EncodeToString(runtime), fmt.Sprintf("%x", digest)}
	return append(command, args...)
}

// FindPython prefers python3 but skips the older Apple system Python when a
// compatible interpreter is installed. No shell startup or environment edits.
func FindPython(ctx context.Context) (string, error) {
	for _, candidate := range []string{"python3", "python3.14", "python3.13", "python3.12", "python3.11", "python3.10", "/opt/homebrew/bin/python3", "/usr/local/bin/python3"} {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = exec.CommandContext(probeCtx, path, "-c", "import sys; sys.exit(sys.version_info < (3,10))").Run()
		cancel()
		if err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("arm calibration needs Python 3.10 or newer; install it to preview locally or use --device")
}

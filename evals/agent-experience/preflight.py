"""Host safety preflight: refuse to run short scenarios on an unsafe host."""
import os
import shutil
import subprocess
import sys
from pathlib import Path

KNOWN_INSTALL_PATHS = ("/usr/bin/wendy", "/usr/local/bin/wendy", "/opt/homebrew/bin/wendy",
                       "~/.local/bin/wendy", "~/go/bin/wendy", "~/bin/wendy")
SHELLS = ("sh", "bash", "zsh", "fish")
NETWORK_PERMISSIONS = ("org.freedesktop.NetworkManager.settings.modify.system",
                       "org.freedesktop.NetworkManager.network-control",
                       "org.freedesktop.NetworkManager.enable-disable-wifi")
# macOS lets a standard user join networks and toggle Wi-Fi unless both are set (airport prefs).
AIRPORT_PREFS = "/Library/Preferences/SystemConfiguration/com.apple.airport.preferences"
AIRPORT_ADMIN_KEYS = ("RequireAdminNetworkChange", "RequireAdminPowerToggle")


def _which(name, env):
    return shutil.which(name, path=env.get("PATH", ""))


def _run(argv, env, timeout):
    try:
        done = subprocess.run(argv, env=env, stdin=subprocess.DEVNULL, capture_output=True,
                              text=True, timeout=timeout)
    except (OSError, subprocess.SubprocessError):
        return None
    return done


PLATFORM = sys.platform  # read at call time, so tests can check the macOS branch on Linux and back
SHELL_TIMEOUT = 10
SUDO_TIMEOUT = 5
NETWORK_TIMEOUT = 10


def login_shell_paths(env):
    names = list(SHELLS)
    shell = env.get("SHELL") or ""
    own = os.path.basename(shell)
    if own and own not in names:
        names.append(own)
    own_exact = os.path.isabs(shell) and os.path.isfile(shell) and os.access(shell, os.X_OK)
    result = {}
    for name in names:
        exe = shell if own_exact and name == own else _which(name, env)
        if exe is None:
            # a standard shell that isn't installed is fine; the user's own shell must be readable
            if name == own:
                result[name] = None
            continue
        if name == "fish":
            argv = [exe, "-l", "-c", "string join : $PATH"]
        else:
            argv = [exe, "-l", "-c", 'printf %s "$PATH"']
        done = _run(argv, env, SHELL_TIMEOUT)
        lines = [] if done is None or done.returncode != 0 else \
            [ln for ln in done.stdout.splitlines() if ln.strip()]
        # profiles may print banners; the PATH is the last line
        result[name] = lines[-1].strip() if lines else None
    return result


def stray_wendy(path_values, shim, home, known_paths, cwd=None):
    shim_real = os.path.realpath(shim)
    candidates = []
    for value in path_values:
        for directory in value.split(os.pathsep):
            # an empty PATH element means the current directory
            candidates.append(os.path.join(directory or cwd or os.getcwd(), "wendy"))
    for known in known_paths:
        if known == "~" or known.startswith("~/"):
            known = str(Path(home) / known[2:])
        candidates.append(known)
    found, seen = [], set()
    for path in candidates:
        real = os.path.realpath(path)
        if os.path.isfile(path) and os.access(path, os.X_OK) and real != shim_real \
                and real not in seen:
            seen.add(real)
            found.append(path)
    return found


def passwordless_sudo(env):
    if _which("sudo", env) is None:
        return False
    done = _run(["sudo", "-n", "true"], env, SUDO_TIMEOUT)
    return done is not None and done.returncode == 0


def macos_network_rights(env):
    done = _run(["id", "-Gn"], env, SUDO_TIMEOUT)
    if done is None or done.returncode != 0:
        return "id -Gn failed"
    if "admin" in done.stdout.split():
        return "admin group"
    # Unverified on a real Mac: `defaults read` of the airport preferences needs no sudo, and a
    # boolean prints as 1. Any key that can't be read, or isn't on, fails closed.
    for key in AIRPORT_ADMIN_KEYS:
        done = _run(["defaults", "read", AIRPORT_PREFS, key], env, NETWORK_TIMEOUT)
        if done is None or done.returncode != 0:
            return "cannot verify Wi-Fi change rights on macOS"
        if done.stdout.strip().lower() not in ("1", "yes", "true"):
            return f"{key} is not enabled in {AIRPORT_PREFS}"
    return None


def network_rights(env):
    """None when this account verifiably can't change network settings, else why it fails."""
    if PLATFORM == "darwin":
        return macos_network_rights(env)
    if _which("nmcli", env) is None:
        return "no NetworkManager to check; cannot verify network rights"
    done = _run(["nmcli", "-t", "-f", "PERMISSION,VALUE", "general", "permissions"],
                env, NETWORK_TIMEOUT)
    if done is None or done.returncode != 0:
        return "nmcli general permissions failed"
    values = {}
    for line in done.stdout.splitlines():
        key, _, value = line.rpartition(":")
        if key:
            values[key] = value.strip()
    if not values:
        return "nmcli general permissions failed"
    for permission in NETWORK_PERMISSIONS:
        if values.get(permission) == "yes":
            return permission
    return None


def check_host(shim, env, home=None, known_paths=KNOWN_INSTALL_PATHS, cwd=None):
    home = Path(home) if home else Path(env.get("HOME") or os.path.expanduser("~"))
    failures = []
    paths = login_shell_paths(env)
    values = [env.get("PATH", "")]
    for name, value in paths.items():
        if value is None:
            failures.append(f"could not read the login PATH of {name}; fix that shell or remove it")
        else:
            values.append(value)
    for path in stray_wendy(values, shim, home, known_paths, cwd):
        failures.append(f"another wendy is reachable at {path}; run as an account that can't see it")
    if passwordless_sudo(env):
        failures.append("sudo works without a password; use an account without passwordless sudo")
    rights = network_rights(env)
    if rights:
        failures.append(f"this account can change network settings ({rights}); "
                        "use an account without that right")
    return failures

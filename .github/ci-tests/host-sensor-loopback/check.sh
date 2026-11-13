#!/bin/sh
# Runs through authenticated `wendy device shell` on the host, never in an app.
# The optional root directory is used only by the local regression fixtures.
set -u

scope=${1:-}
root=${2:-}
case "$scope" in
    video|video-audio) ;;
    *) echo "FAIL: expected video or video-audio scope"; exit 2 ;;
esac

failures=0
fail() {
    echo "FAIL: $*"
    failures=$((failures + 1))
}

if [ "$(uname -s)" != Linux ]; then
    echo "FAIL: loopback check must run on the Linux device host"
    exit 1
fi
if [ "$(id -u)" != 0 ]; then
    echo "FAIL: authenticated device shell must run as root to load host modules"
    exit 1
fi
for tool in modinfo modprobe timeout; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        fail "required host tool $tool is unavailable"
    fi
done
[ "$failures" -eq 0 ] || exit 1

release=$(uname -r)
echo "Host kernel: $release; required loopback support: $scope"

module_present() {
    module=$1
    if ! path=$(modinfo -k "$release" -F filename "$module" 2>&1); then
        fail "$module is missing for host kernel $release: $path"
        return 1
    fi
    case "$path" in
        "$root/lib/modules/$release/"*|"$root/usr/lib/modules/$release/"*) ;;
        *) fail "$module resolves outside the running kernel's module tree: $path"; return 1 ;;
    esac
    if [ ! -s "$path" ]; then
        fail "$module has no installed module file at $path"
        return 1
    fi
    vermagic=$(modinfo -k "$release" -F vermagic "$module" 2>/dev/null) || vermagic=""
    if [ "${vermagic%% *}" != "$release" ]; then
        fail "$module vermagic '$vermagic' does not match host kernel $release"
        return 1
    fi
    echo "OK: $module installed at $path"
}

wait_for_character_device() {
    attempt=0
    while [ "$attempt" -lt 30 ]; do
        [ -c "$1" ] && return 0
        sleep 0.1
        attempt=$((attempt + 1))
    done
    fail "missing host character device $1"
    return 1
}

if module_present v4l2loopback; then
    version=$(modinfo -k "$release" -F version v4l2loopback 2>/dev/null) || version=""
    case "$version" in
        0.15.*)
            if timeout 15 modprobe v4l2loopback devices=0 exclusive_caps=1; then
                loaded_version=$(cat "$root/sys/module/v4l2loopback/version" 2>/dev/null) || loaded_version=""
                if [ "$loaded_version" != "$version" ]; then
                    fail "loaded v4l2loopback version '$loaded_version' differs from installed '$version'"
                fi
                wait_for_character_device "$root/dev/v4l2loopback" || :
            else
                fail "v4l2loopback cannot load into host kernel $release"
            fi
            ;;
        *) fail "v4l2loopback version '$version' does not support the agent's 0.15.x control ABI" ;;
    esac
fi

if [ "$scope" = video-audio ] && module_present snd-aloop; then
    if timeout 15 modprobe snd-aloop; then
        if [ ! -d "$root/sys/module/snd_aloop" ]; then
            fail "modprobe returned success but snd-aloop is not loaded in the host kernel"
        fi
        loopback_card=""
        for card_id in "$root"/sys/class/sound/card*/id; do
            [ -f "$card_id" ] || continue
            if [ "$(cat "$card_id")" = Loopback ]; then
                card_dir=${card_id%/id}
                loopback_card=${card_dir##*/card}
                break
            fi
        done
        if [ -z "$loopback_card" ]; then
            fail "snd-aloop loaded but the host has no ALSA Loopback card"
        else
            wait_for_character_device "$root/dev/snd/pcmC${loopback_card}D0p" || :
            wait_for_character_device "$root/dev/snd/pcmC${loopback_card}D1c" || :
        fi
    else
        fail "snd-aloop cannot load into host kernel $release"
    fi
fi

if [ "$failures" -ne 0 ]; then
    echo "FAIL: host loopback support has $failures error(s)"
    exit 1
fi
echo "PASS: host sensor loopback support ($scope)"

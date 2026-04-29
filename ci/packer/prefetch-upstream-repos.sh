#!/usr/bin/env bash
#
# Pre-clone the upstream Yocto layer repos pinned in scripts/upstream-repos.env
# into a cache directory. Invoked by ci/packer/wendyos-builder.pkr.hcl while
# baking the CI runner AMI; bootstrap.sh's clone_repos picks them up and
# fetches/checks out instead of doing a fresh clone.
#
# Usage:
#   prefetch-upstream-repos.sh <target-dir> <upstream-repos.env path>

set -euo pipefail

target_dir="${1:?target dir required}"
env_file="${2:?upstream-repos.env path required}"

mkdir -p "${target_dir}"
cd "${target_dir}"

# shellcheck disable=SC1090
source "${env_file}"

# Mirror the (URL, SRCREV, folder) tuples that bootstrap.sh derives from
# repos[]. Folder name = basename of URL with .git stripped, matching
# clone_repos in bootstrap.sh.
declare -A repos=(
    [poky]="${URL_POKY}|${SRCREV_POKY}"
    [meta-openembedded]="${URL_OE}|${SRCREV_OE}"
    [meta-tegra]="${URL_TEGRA}|${SRCREV_TEGRA}"
    [meta-tegra-community]="${URL_TEGRA_COMM}|${SRCREV_TEGRA_COMM}"
    [meta-virtualization]="${URL_VIRT}|${SRCREV_VIRT}"
    [meta-mender]="${URL_MENDER}|${SRCREV_MENDER}"
    [meta-mender-community]="${URL_MENDER_COMM}|${SRCREV_MENDER_COMM}"
    [meta-raspberrypi]="${URL_RPI}|${SRCREV_RPI}"
)

for folder in "${!repos[@]}"; do
    IFS='|' read -r url srcrev <<< "${repos[$folder]}"
    printf '[prefetch] %s @ %s\n' "${folder}" "${srcrev}"

    if [[ ! -d "${folder}/.git" ]]; then
        git clone "${url}" "${folder}"
    fi

    (
        cd "${folder}"
        git fetch --tags origin
        git checkout --detach "${srcrev}"
        git gc --auto
    )
done

# Make the cache world-readable so any user the runner spins up under
# (RunsOn defaults to `runner`) can copy / clone from it without sudo.
chmod -R a+rX "${target_dir}"

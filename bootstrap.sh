#!/usr/bin/env bash

set -e          # abort on errors (nonzero exit code)
set -u          # detect unset variable usages
set -o pipefail # abort on errors within pipes
#set -x         # logs raw input, including unexpanded variables and comments

#trap "echo 'error: Script failed: see failed command above'" ERR

###
# Get absolute path in a portable way (works on Linux and macOS)
absolute_path() {
    local path="${1}"

    if [ -z "${path}" ]
    then
        return 1
    fi

    # Try different methods in order of preference
    if command -v realpath >/dev/null 2>&1
    then
        # Linux and macOS Ventura+
        realpath "${path}"
    elif command -v greadlink >/dev/null 2>&1
    then
        # GNU readlink from coreutils (brew install coreutils on macOS)
        greadlink -f "${path}"
    elif [[ "$(uname)" == "Darwin" ]] && readlink -f / >/dev/null 2>&1
    then
        # macOS Monterey 12.3+ with readlink -f support
        readlink -f "${path}"
    else
        # Fallback:
        # Use cd + pwd for absolute path resolution
        # (supported on) all POSIX systems)
        (cd -P -- "${path}" 2>/dev/null && pwd -P) || {
            echo "Error: Cannot resolve absolute path for: ${path}" >&2
            return 1
        }
    fi
}

# folder where the script is located
HOME_DIR="$(absolute_path "${0%/*}")"
# printf "HOME_DIR: %s\n" "${HOME_DIR}"

# folder from which the script was called
WORK_DIR="$(pwd)"

IMAGE_NAME="wendyos"
USER_NAME="dev"
# PROJECT_DIR="${1:-${ROOT_DIR}}"
PROJECT_DIR="${WORK_DIR}"
LOG_FILE="${WORK_DIR}/yocto_setup.log"
META_LAYER_DIR="${HOME_DIR}"
DOCKER_WORK_DIR="/home/${USER_NAME}/${IMAGE_NAME}"


YOCTO_BRANCH="scarthgap"
YOCTO_BUILD_DIR="build"

cleanup() {
    # preserve original exit code
    rc=$?
    cd -- "${WORK_DIR}" || true
    exit "${rc}"
}
trap cleanup EXIT

# Default repo URLs and commit hashes live in scripts/upstream-repos.env so the
# CI runner AMI (built by ci/packer/*.pkr.hcl) can prime the same revisions
# without re-declaring them. A per-board conf/template/boards/<board-id>/
# repos.overrides file may override any URL_*/SRCREV_* (and append entries via
# REPOS_EXTRA) before repos[] is built below.
# shellcheck source=scripts/upstream-repos.env
source "${HOME_DIR}/scripts/upstream-repos.env"


##
# display help
usage() {
    cat <<EOF
Usage:
  BOARD=<board-id> $(basename "${0}") [options]

Example:
  BOARD=jetson-agx-orin $(basename "${0}")
  BOARD=rpi5-nvme $(basename "${0}")

Environment variables:
  BOARD     (required) Target board id. Must match a directory
            conf/template/boards/<board-id>/ containing local.conf and
            bblayers.conf. Those files pull in shared fragments from
            conf/template/include/{local,bblayers}/ via BitBake 'require'.
            Run with an unknown BOARD to see the list of supported board ids.
  MACHINE   Deprecated alias for BOARD. Prints a warning on use.
            Separate from bitbake's MACHINE (the yocto machine name) --
            rename scheduled to avoid confusion.
  WENDYOS_HOST_BUILD
            When set to 1, skip the Docker build environment and prepare the
            tree for a host-native bitbake invocation. Used by CI runners that
            boot from a custom AMI with the build prerequisites preinstalled.
  WENDYOS_REPO_CACHE_DIR
            Optional path to a directory with pre-cloned upstream layer repos
            (poky/, meta-tegra/, ...). When set, missing entries in repos/ are
            seeded from this cache before clone_repos runs, turning each clone
            into a cheap fetch + checkout. Used by the CI AMI.

Options:
  --help, -h   Show this help message.

EOF
}

###
# Parse command-line arguments
for arg in "$@"; do
    case "${arg}" in
        --help|-h)
            usage
            exit 0
            ;;
        *)
            printf "Unknown argument: %s\n" "${arg}" >&2
            usage
            exit 1
            ;;
    esac
done

# Accept BOARD as the primary env var, with MACHINE as a deprecated alias.
# MACHINE collides with bitbake's MACHINE (the yocto machine name), which is
# a different concept; BOARD is the board-id used to look up the template.
if [[ -z "${BOARD:-}" ]] && [[ -n "${MACHINE:-}" ]]
then
    printf "WARN: MACHINE= is deprecated as a bootstrap argument. Use BOARD= instead.\n" >&2
    BOARD="${MACHINE}"
fi

if [[ -z "${BOARD:-}" ]]; then
    printf "ERROR: BOARD environment variable is required.\n" >&2
    printf "       Set it to a board id matching a directory in conf/template/boards/<board-id>/.\n" >&2
    usage
    exit 1
fi

invalid_folder_structure() {
    local -r work_dir="${1}"
    local -r meta_dir="${2}"

    cat <<EOF >&2
ERROR: 'meta-${IMAGE_NAME}' must be located within the working directory subtree.

Current locations:
  Working directory:     ${work_dir}
  meta-${IMAGE_NAME} location:  ${meta_dir}

The bootstrap script creates a Docker container that mounts the working directory.
If 'meta-${IMAGE_NAME}' is outside this directory, it will not be accessible in the container.

Recommended actions:
  1. Clone or move meta-${IMAGE_NAME} inside the working directory
  2. Run the bootstrap script from a parent directory that contains meta-${IMAGE_NAME}

Example structure:
  /path/to/project         <- run bootstrap.sh from here
  ├── meta-${IMAGE_NAME}          <- meta layer repository
  ├── repos                <- created by bootstrap
  ├── build                <- created by bootstrap
  └── docker               <- created by bootstrap

EOF
}

###
# Check if meta layer is within the WORK_DIR subtree
validate_meta_location() {
    local work_dir
    local meta_dir

    work_dir="$(absolute_path "${WORK_DIR}")" || return 1
    meta_dir="$(absolute_path "${META_LAYER_DIR}")" || return 1

    # Check if meta layer path starts with WORK_DIR path
    case "${meta_dir}" in
        "${work_dir}"*)
            # meta layer is inside WORK_DIR subtree
            return 0
            ;;
        *)
            # meta layer is outside WORK_DIR subtree
            invalid_folder_structure "${work_dir}" "${meta_dir}"
            return 1
            ;;
    esac
}

###
# Resolve a git ref (branch, tag, or commit) to its commit hash
# Works with local refs, remote refs, or returns the input if already a hash
resolve_ref() {
    local ref="${1}"
    local resolved

    if resolved=$(git rev-parse --verify "${ref}" 2>/dev/null); then
        echo "${resolved}"
    elif resolved=$(git rev-parse --verify "origin/${ref}" 2>/dev/null); then
        echo "${resolved}"
    else
        # assume it's already a commit hash
        echo "${ref}"
    fi
}

###
function clone_repos() {
    for repo in "${repos[@]}"
    do
        local enable
        local url
        local folder
        local srcrev

        enable=$(echo "${repo}" | cut -d'|'  -f 1)
        [ "${enable}" -ne 1 ] && {
            continue
        }

        url=$(echo "${repo}" | cut -d'|'  -f 2)
        folder=$(echo "${repo}" | cut -d'|'  -f 3)
        [[ -z "${folder}" ]] && {
            folder=$(basename "${url%.git}")
        }

        srcrev=$(echo "${repo}" | cut -d'|'  -f 4)
        [[ -z "${srcrev}" ]] && {
            printf "No SRCREV for '%s'\n" "${url}"
            return 1
        }

        # check if repo already exists
        if [[ -d "./${folder}" ]]; then
            # repo exists - verify it's at the correct revision
            cd "${folder}"

            # fetch latest refs from remote
            git fetch origin >> "${LOG_FILE}" 2>&1 || {
                printf "[error] Failed to fetch '%s'\n" "${folder}"
                cd ..
                return 1
            }

            # check if the repo is already at target revision
            local target_commit
            local current_head

            target_commit=$(resolve_ref "${srcrev}")
            current_head=$(git rev-parse HEAD 2>/dev/null) || {
                printf "[error] Cannot determine HEAD in '%s'\n" "${folder}"
                cd ..
                return 1
            }

            if [[ "${current_head}" == "${target_commit}" ]]; then
                #already at correct revision - skip
                printf "[ok] '%s' at %s\n" "${folder}" "${srcrev}"
                cd ..
                continue
            fi

            # need to update to target revision
            printf "[update] '%s' to %s\n" "${folder}" "${srcrev}"
        else
            # repo doesn't exist - clone it
            printf "[clone] '%s' at %s\n" "${url}" "${srcrev}"
            git clone "${url}" "${folder}" >> "${LOG_FILE}" 2>&1 || {
                return 1
            }

            cd "${folder}"
        fi

        # we need to checkout (either new clone or update)
        git checkout "${srcrev}" >> "${LOG_FILE}" 2>&1 || {
            printf "[error] Failed to checkout %s in '%s'\n" "${srcrev}" "${folder}"
            cd ..
            return 1
        }

        cd ..
    done
}

copy_dir() {
    local src="${1}"
    local dst="${2}"

    if [ -z "${src}" ] || [ -z "${dst}" ]; then
        echo "Usage: copy_dir <source_dir> <dest_dir>" >&2
        return 2
    fi

    if [ ! -d "${src}" ]; then
        echo "Source is not a directory: ${src}" >&2
        return 1
    fi

    # Ensure destination exists
    mkdir -p -- "${dst}" || return $?

    if command -v ditto >/dev/null 2>&1; then
        # Best on macOS: preserves permissions, ACLs, xattrs, symlinks
        ditto "${src}" "${dst}"
    elif command -v rsync >/dev/null 2>&1; then
        # Cross-platform: preserves perms, times, symlinks, devices, etc.
        # Trailing slashes copy contents of src into dst
        rsync -aH -- "${src}"/ "${dst}"/
    else
        # POSIX fallback (may not keep ACLs/xattrs)
        cp -Rpv -- "${src}"/. "${dst}"/
    fi
}

# Validate that meta layer is within WORK_DIR subtree
printf "Validating meta-${IMAGE_NAME} location...\n"
validate_meta_location || {
    exit 1
}

[[ ! -d "${PROJECT_DIR}" ]] && {
    mkdir -p "${PROJECT_DIR}"
}

cd "${PROJECT_DIR}"
mkdir -p "repos"
cd "repos"

# Seed repos/ from a pre-cloned cache when one is provided. The CI runner AMI
# (built by ci/packer/wendyos-builder.pkr.hcl) bakes the upstream layer
# repos at the pinned SRCREVs into /opt/wendyos-cache/repos so clone_repos
# below sees them as already-checked-out and only runs `git fetch` + `git
# checkout`. Local dev leaves this unset and clones fresh as before.
if [[ -n "${WENDYOS_REPO_CACHE_DIR:-}" && -d "${WENDYOS_REPO_CACHE_DIR}" ]]
then
    printf "Seeding repos/ from cache: %s\n" "${WENDYOS_REPO_CACHE_DIR}"
    for cached in "${WENDYOS_REPO_CACHE_DIR}"/*/
    do
        [[ -d "${cached}" ]] || continue
        folder=$(basename "${cached}")
        if [[ ! -d "./${folder}" ]]
        then
            cp -r "${cached}" "./${folder}"
        fi
    done
fi

# Resolve template files based on BOARD. Each board has its own directory
# conf/template/boards/<board-id>/ containing a self-contained local.conf and
# bblayers.conf, which pull in shared fragments from
# conf/template/include/{local,bblayers}/ via BitBake 'require'.
TEMPLATE_DIR="${META_LAYER_DIR}/conf/template"
BOARD_DIR="${TEMPLATE_DIR}/boards/${BOARD}"

if [[ ! -d "${BOARD_DIR}" ]]
then
    printf "ERROR: Unknown board '%s'. Available boards:\n" "${BOARD}" >&2
    for d in "${TEMPLATE_DIR}"/boards/*/
    do
        [[ -d "${d}" ]] || continue
        printf "    %s\n" "$(basename "${d}")" >&2
    done
    exit 1
fi

# Per-board repo overrides (optional): override URL_*/SRCREV_* defaults
# and/or append to REPOS_EXTRA before repos[] is built.
if [[ -f "${BOARD_DIR}/repos.overrides" ]]
then
    # shellcheck source=/dev/null
    source "${BOARD_DIR}/repos.overrides"
fi

# Build the repos list with the (possibly overridden) URLs and SRCREVs.
# Indexed (not associative) so iteration preserves the order below.
declare -a repos=(
    "1|${URL_POKY}||${SRCREV_POKY}"
    "1|${URL_OE}||${SRCREV_OE}"
    "1|${URL_TEGRA}||${SRCREV_TEGRA}"
    "1|${URL_TEGRA_COMM}||${SRCREV_TEGRA_COMM}"
    "1|${URL_VIRT}||${SRCREV_VIRT}"
    "1|${URL_MENDER}||${SRCREV_MENDER}"
    "1|${URL_MENDER_COMM}||${SRCREV_MENDER_COMM}"
    "1|${URL_RPI}||${SRCREV_RPI}"
)

# Append any extras declared by the override file.
if [[ -n "${REPOS_EXTRA+x}" ]]
then
    repos+=("${REPOS_EXTRA[@]}")
fi

printf "Clone repos...\n"
clone_repos || {
    printf "Yocto setup failed!\n"
    cd "${WORK_DIR}"
    exit 1
}

image_name=$(basename "${META_LAYER_DIR}")

printf "\nPrepare the Yocto build environment...\n"
cd "${PROJECT_DIR}"
mkdir -p "${YOCTO_BUILD_DIR}/conf"

for f in local.conf bblayers.conf
do
    src="${BOARD_DIR}/${f}"
    if [[ ! -f "${src}" ]]
    then
        printf "ERROR: Missing %s in %s\n" "${f}" "${BOARD_DIR}" >&2
        exit 1
    fi
done

# Only overwrite if the build dir doesn't already have the file
# (matches previous behavior — user edits to build/conf survive re-bootstrap).
# WENDYOS_META_REPO is prepended only to bblayers.conf (parsed first by BitBake);
# the value stays in scope when local.conf is parsed later.
for f in local.conf bblayers.conf
do
    dst="./${YOCTO_BUILD_DIR}/conf/${f}"
    if [[ ! -e "${dst}" ]]
    then
        if [[ "${f}" == "bblayers.conf" ]]
        then
            {
                printf 'WENDYOS_META_REPO = "%s"\n\n' "${image_name}"
                cat "${BOARD_DIR}/${f}"
            } > "${dst}"
        else
            cp "${BOARD_DIR}/${f}" "${dst}"
        fi
    fi
done

printf "\nDirectory structure:\n"
tree -d -L 2 -I 'build|downloads|sstate-cache' || true #--charset=ascii

# Host-build mode: skip the Docker image entirely. CI runs on an AMI that
# already has the build prerequisites installed (see ci/packer/), and a
# disposable VM doesn't benefit from the container's isolation. Local dev
# leaves WENDYOS_HOST_BUILD unset and gets the Docker flow as before.
if [[ "${WENDYOS_HOST_BUILD:-0}" == "1" ]]
then
    cd "${WORK_DIR}"
    cat <<EOF

Bootstrap complete (host-build mode, no Docker image was created).

Build with:
   make build MACHINE=<machine-name> WENDYOS_HOST_BUILD=1

Or directly:
   . ./repos/poky/oe-init-build-env ${YOCTO_BUILD_DIR}
   bitbake wendyos-image

EOF
    exit 0
fi

# prepare Docker image
printf "\nCreate docker image...\n"
docker_path="${PROJECT_DIR}/docker"
mkdir -p "${docker_path}"
copy_dir "${META_LAYER_DIR}/scripts/docker" "${docker_path}"

# Stage the shared package install script into the Docker build context.
# Same script is consumed by ci/packer/wendyos-builder.pkr.hcl so the dev
# container and the CI AMI install an identical set of build prerequisites.
mkdir -p "${docker_path}/files"
cp "${META_LAYER_DIR}/scripts/install-build-deps.sh" "${docker_path}/files/install-build-deps.sh"
chmod +x "${docker_path}/files/install-build-deps.sh"

sed -i.bak "s|%HOST_DIR%|${PROJECT_DIR}|g" "${docker_path}/dockerfile.config"
sed -i.bak "s|%OS_NAME%|${IMAGE_NAME}|g" "${docker_path}/dockerfile.config"
rm -f "${docker_path}/dockerfile.config.bak"

cd "${PROJECT_DIR}/docker"
./docker-util.sh create

cd "${WORK_DIR}"
cat <<EOF

Run the following command(s):
   # start the Docker container
   cd ./docker
   ./docker-util.sh run

   # (within Docker container)
   cd ./${IMAGE_NAME}
   . ./repos/poky/oe-init-build-env ${YOCTO_BUILD_DIR}
   bitbake wendyos-image

EOF

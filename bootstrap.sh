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

SRCREV_POKY="353491479086e8d3f209d5cce0019a29e143b064"
SRCREV_OE="2759d8870ea387b76c902070bed8a6649ff47b56"
SRCREV_TEGRA="447c21467f65be2389f68a189b6871f13729d222"
SRCREV_TEGRA_COMM="241d1073ba8e610ef8da3fe8470b0a4d0567521f"
SRCREV_VIRT="f92518e20530edfebca45e4170e11460949a5303"
SRCREV_MENDER="76404a7b914676a57d76ccb5fe12149112c05c03"
SRCREV_MENDER_COMM="9145b8e34bac23c82984ddcdd5468154ffe7af6d"
SRCREV_RPI="3afc9728b1f4ba0f5be1af34883d6582966133a1"

declare -Ar repos=(
    [0]="1|git://git.yoctoproject.org/poky.git||${SRCREV_POKY}"
    [1]="1|https://github.com/openembedded/meta-openembedded.git||${SRCREV_OE}"
    [2]="1|https://github.com/OE4T/meta-tegra.git||${SRCREV_TEGRA}"
    [3]="1|https://github.com/OE4T/meta-tegra-community||${SRCREV_TEGRA_COMM}"
    [4]="1|git://git.yoctoproject.org/meta-virtualization.git||${SRCREV_VIRT}"
    [5]="1|https://github.com/mendersoftware/meta-mender.git||${SRCREV_MENDER}"
    [6]="1|https://github.com/mendersoftware/meta-mender-community.git||${SRCREV_MENDER_COMM}"
    [7]="1|https://github.com/agherzan/meta-raspberrypi.git||${SRCREV_RPI}"
)


##
# display help
usage() {
    cat <<EOF
Usage:
  MACHINE=<machine> $(basename "${0}") [options]

Example:
  MACHINE=rpi5 $(basename "${0}")
  MACHINE=jetson $(basename "${0}")

Environment variables:
  MACHINE   (required) Target machine. Selects conf/template/bblayers.conf.<MACHINE>
            and conf/template/local.conf.<MACHINE>.
            Available machines: jetson, rpi5, qemu
            Note: for RPi5 NVMe boot, use MACHINE=rpi5 then set
            MACHINE = "raspberrypi5-nvme-wendyos" in build/conf/local.conf.

Options:
  --help, -h   Show this help message.

EOF
}

trim() {
    local s="${1}"

    # remove leading whitespace
    s="${s#"${s%%[![:space:]]*}"}"

    # remove trailing whitespace
    s="${s%"${s##*[![:space:]]}"}"

    printf '%s' "${s}"
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

if [[ -z "${MACHINE:-}" ]]; then
    printf "ERROR: MACHINE environment variable is required.\n" >&2
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

# Resolve template file names based on MACHINE environment variable
BBLAYERS_SRC="${META_LAYER_DIR}/conf/template/bblayers.conf.${MACHINE}"
LOCAL_SRC="${META_LAYER_DIR}/conf/template/local.conf.${MACHINE}"

if [[ ! -f "${BBLAYERS_SRC}" ]] || [[ ! -f "${LOCAL_SRC}" ]]; then
    printf "ERROR: No template files found for machine '%s'.\n" "${MACHINE}" >&2
    printf "  Expected:\n" >&2
    printf "    %s\n" "${BBLAYERS_SRC}" >&2
    printf "    %s\n" "${LOCAL_SRC}" >&2
    printf "  Available machines:\n" >&2
    for f in "${META_LAYER_DIR}/conf/template/bblayers.conf."*; do
        suffix="${f##*.conf.}"
        if [[ -f "${META_LAYER_DIR}/conf/template/local.conf.${suffix}" ]]; then
            printf "    %s\n" "${suffix}" >&2
        fi
    done
    exit 1
fi

# use the template only if the corresponding one in build/conf doesn't exist
if [[ ! -e "./${YOCTO_BUILD_DIR}/conf/bblayers.conf" ]]
then
    cp "${BBLAYERS_SRC}" "./${YOCTO_BUILD_DIR}/conf/bblayers.conf"
    sed -i.bak "s|%META-REPO%|${image_name}|g" "./${YOCTO_BUILD_DIR}/conf/bblayers.conf"
    rm -f "./${YOCTO_BUILD_DIR}/conf/bblayers.conf.bak"
fi

if [[ ! -e "./${YOCTO_BUILD_DIR}/conf/local.conf" ]]
then
    cp "${LOCAL_SRC}" "./${YOCTO_BUILD_DIR}/conf/local.conf"
fi

printf "\nDirectory structure:\n"
tree -d -L 2 -I 'build|downloads|sstate-cache' || true #--charset=ascii

# prepare Docker image
printf "\nCreate docker image...\n"
docker_path="${PROJECT_DIR}/docker"
mkdir -p "${docker_path}"
copy_dir "${META_LAYER_DIR}/scripts/docker" "${docker_path}"

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

#!/usr/bin/env bash

#
# ./docker-util.sh --create --config=dockerfile --image=<image_name>
# ./docker-util.sh --run --image=<image_name> --in="output" --out="/home/phoenix/yocto"
# ./docker-util.sh --remove --image=<image_name>
#

set -euo pipefail

# colored console support
# Black        0;30     Dark Gray     1;30
# Red          0;31     Light Red     1;31
# Green        0;32     Light Green   1;32
# Brown/Orange 0;33     Yellow        1;33
# Blue         0;34     Light Blue    1;34
# Purple       0;35     Light Purple  1;35
# Cyan         0;36     Light Cyan    1;36
# Light Gray   0;37     White         1;37
CON_SWITCH="\033["
CON_RESET="${CON_SWITCH}0m"
# CON_RED="${CON_SWITCH}0;31m"
CON_LRED="${CON_SWITCH}1;31m"
# CON_GREEN="${CON_SWITCH}0;32m"
# CON_LGREEN="${CON_SWITCH}1;32m"
# CON_ORANGE="${CON_SWITCH}0;33m"
# CON_YELLOW="${CON_SWITCH}1;33m"
# CON_BLUE="${CON_SWITCH}0;34m"
# CON_LBLUE="${CON_SWITCH}1;34m"
# CON_LCYAN="${CON_SWITCH}1;36m"
# CON_GRAY="${CON_SWITCH}1;30m"
# CON_LGRAY="${CON_SWITCH}0;37m"
# CON_WHITE="${CON_SWITCH}1;37m"

DOCKER_ARGS=""

###
function parse_config() {
    local config_file="$1"
    local src
    local dest
    local rights

    while read -r line; do
        [[ "$line" =~ ^#.*$ ]] && {
            #ignore comments
            continue
        }

        [[ -z "$line" ]] && {
            #ignore empty lines
            continue
        }

        src=$(echo "${line}" | cut -d':' -f 1)
        src=$(eval echo "${src}")

        dest=$(echo "${line}" | cut -s -d':' -f 2)
        dest=$(eval echo "${dest}")

        rights=$(echo "${line}" | cut -s -d':' -f 3)
        rights=$(eval echo "${rights}")

        # printf "Host: '%s'\n" "${src}"
        # printf "Docker: '%s'\n" "${dest}"
        # [ ! -d "${src}" ] && {
        #     mkdir -p "${src}"
        # }

        DOCKER_ARGS="${DOCKER_ARGS} -v ${src}:${dest}"
        [[ -n "${rights}" ]] && {
            DOCKER_ARGS="${DOCKER_ARGS}:${rights}"
        }
    done < "${config_file}"
}

# folder where the script is located
HOME_DIR="$(realpath "${0%/*}")"

# folder from which the script was called
WORK_DIR="$(pwd)"

declare -A opts=(
    [create]=0
    [remove]=0
)

OS_NAME="wendyos"
DOCKER_BASE="${DOCKER_BASE:-ubuntu:24.04}"
DOCKER_NAME=""
DOCKER_REPO="${DOCKER_REPO:-${OS_NAME}-build}"
# DOCKER_TAG="latest"
DOCKER_TAG="${DOCKER_TAG:-scarthgap}"
DOCKER_CONFIG=""
DOCKER_ENV="${WORK_DIR}/environment"
DOCKER_USER="dev"
DOCKER_HOST="yocto"
# Docker named volumes for macOS case-sensitive storage.
# Volume names must match the Makefile (VOLUME_BUILD, etc.).
VOLUME_BUILD="${OS_NAME}-build-tmp"
VOLUME_SSTATE="${OS_NAME}-sstate-cache"
VOLUME_DOWNLOADS="${OS_NAME}-downloads"
VOLUME_CACHE="${OS_NAME}-build-cache"

###
# On macOS, the host filesystem (APFS) is case-insensitive by default.
# Yocto requires a case-sensitive filesystem for TMPDIR. Docker named volumes
# use ext4 inside the Docker VM, which is case-sensitive.
# This function creates the volumes if they don't exist, fixes ownership, and
# appends the additional -v flags to mount them over the bind-mounted paths.
ensure_macos_volumes() {
    if [ "$(uname)" != "Darwin" ]; then
        return
    fi

    # Compute workdir here (after argument parsing, so --user= is applied)
    local workdir="/home/${DOCKER_USER}/${OS_NAME}"

    printf "macOS detected: using Docker named volumes for case-sensitive storage\n"

    local volumes=("${VOLUME_BUILD}" "${VOLUME_SSTATE}" "${VOLUME_DOWNLOADS}" "${VOLUME_CACHE}")
    for vol in "${volumes[@]}"; do
        if ! docker volume inspect "${vol}" >/dev/null 2>&1; then
            printf "  Creating Docker volume: %s\n" "${vol}"
            docker volume create "${vol}" >/dev/null
        fi
    done

    # Fix ownership on volumes so the container user can write to them.
    # Runs every time (idempotent) — handles the case where volumes were
    # created before the Docker image existed (chown couldn't run then).
    if docker_image_exists "${DOCKER_REPO}" "${DOCKER_TAG}"; then
        printf "  Ensuring volume ownership for user %s (uid=%s)...\n" "${DOCKER_USER}" "$(id -u)"
        docker run --rm \
            -v "${VOLUME_BUILD}":"${workdir}/build/tmp" \
            -v "${VOLUME_SSTATE}":"${workdir}/sstate-cache" \
            -v "${VOLUME_DOWNLOADS}":"${workdir}/downloads" \
            -v "${VOLUME_CACHE}":"${workdir}/build/cache" \
            "${DOCKER_REPO}:${DOCKER_TAG}" \
            chown "$(id -u):$(id -g)" \
                "${workdir}/build/tmp" \
                "${workdir}/sstate-cache" \
                "${workdir}/downloads" \
                "${workdir}/build/cache"
    fi

    DOCKER_ARGS="${DOCKER_ARGS} -v ${VOLUME_BUILD}:${workdir}/build/tmp"
    DOCKER_ARGS="${DOCKER_ARGS} -v ${VOLUME_SSTATE}:${workdir}/sstate-cache"
    DOCKER_ARGS="${DOCKER_ARGS} -v ${VOLUME_DOWNLOADS}:${workdir}/downloads"
    DOCKER_ARGS="${DOCKER_ARGS} -v ${VOLUME_CACHE}:${workdir}/build/cache"
}

cleanup() {
    # preserve original exit code
    rc=$?
    cd -- "${WORK_DIR}" || true
    exit "$rc"
}
trap cleanup EXIT

##
# display help
usage() {
    cat <<EOF

Usage:
  $(basename "$0") [options] command

Options:
  --name=<name>         Docker container name (optional)
  --repo=<name>         Docker repository name [${DOCKER_REPO}]
  --tag=<name>          Docker tag name [${DOCKER_TAG}]
  --user                User name running on Docker container [${DOCKER_USER}]
  --host                Docker host name [${DOCKER_HOST}]

Command(s):
  create        Build Docker image
  run           Run the docker container
  remove        Remove Docker image

[Examples]
(using the default configuration)

Create an image:
  $(basename $0) create

Run an image:
  $(basename $0) run

EOF
}

# Returns 0 (true) if the specific repo:tag image exists locally, else 1 (false).
docker_image_exists() {
    local repo="$1"
    local tag="$2"

    # require both args
    if [[ -z "$repo" || -z "$tag" ]]; then
        return 2
    fi

    if [ -n "$(docker image ls -q --filter "reference=${repo}:${tag}")" ]; then
        return 0
    fi

    return 1
}

### main
[ $# -eq 0 ] && {
    usage
    exit 0
}

cmd_parsed=0

# parse command line arguments...
while [ "$#" -gt 0 ]; do
    arg=$1
    shift

    [[ 1 -eq "${cmd_parsed}" ]] && {
        # in case multiple commands are provided, only the first one is considered
        break
    }

    # additional argument processing...
    case ${arg} in
    create)
        opts[create]=1
        cmd_parsed=1
        ;;

    run)
        opts[create]=0
        cmd_parsed=1
        ;;

    remove)
        opts[remove]=1
        cmd_parsed=1
        ;;

    --name=*)
        if [[ "${arg}" == *"="* ]]
        then
            tmp=${arg##*=}
            [ ! -z "${tmp}" ] && {
                DOCKER_NAME="${tmp}"
            }
        fi
        ;;

    --repo=*)
        if [[ "${arg}" == *"="* ]]
        then
            tmp=${arg##*=}
            [ ! -z "${tmp}" ] && {
                DOCKER_REPO="${tmp}"
            }
        fi
        ;;

    --tag=*)
        if [[ "${arg}" == *"="* ]]
        then
            tmp=${arg##*=}
            [ ! -z "${tmp}" ] && {
                DOCKER_TAG="${tmp}"
            }
        fi
        ;;

    --user=*)
        if [[ "${arg}" == *"="* ]]
        then
            tmp=${arg##*=}
            [ ! -z "${tmp}" ] && {
                DOCKER_USER="${tmp}"
            }
        fi
        ;;

    --host=*)
        if [[ "${arg}" == *"="* ]]
        then
            tmp=${arg##*=}
            [ ! -z "${tmp}" ] && {
                DOCKER_HOST="${tmp}"
            }
        fi
        ;;

    h|--help)
        usage
        exit 0
        ;;

    *)
        printf "error: unknown argument '%s'\n" "${arg}"
        exit 1
        ;;
    esac
done

[ -z "${DOCKER_REPO}" ] && {
    printf "${CON_LRED}Error${CON_RESET}: Docker image name not provided\n"
    exit 1
}

[ 1 -eq "${opts[remove]}" ] && {
    if docker_image_exists "${DOCKER_REPO}" "${DOCKER_TAG}"; then
        printf "Remove Docker image... (%s:%s)\n" "${DOCKER_REPO}" "${DOCKER_TAG}"
        docker image rm "${DOCKER_REPO}:${DOCKER_TAG}" || printf "  Warning: could not remove image (may be in use by a container)\n"
    else
        printf "Docker image not found (%s:%s), nothing to remove.\n" "${DOCKER_REPO}" "${DOCKER_TAG}"
    fi

    if [ "$(uname)" = "Darwin" ]; then
        mac_volumes=("${VOLUME_BUILD}" "${VOLUME_SSTATE}" "${VOLUME_DOWNLOADS}" "${VOLUME_CACHE}")
        for vol in "${mac_volumes[@]}"; do
            if docker volume inspect "${vol}" >/dev/null 2>&1; then
                printf "Remove Docker volume: %s\n" "${vol}"
                docker volume rm "${vol}" || printf "  Warning: could not remove %s (may be in use by a container)\n" "${vol}"
            fi
        done
    fi

    exit 0
}

if [[ 1 -eq "${opts[create]}" ]]
then
    if docker_image_exists "${DOCKER_REPO}" "${DOCKER_TAG}"
    then
        printf "Docker image already exists (%s:%s)\n" "${DOCKER_REPO}" "${DOCKER_TAG}"
        exit 0
    fi

    DOCKER_CONFIG="${WORK_DIR}/dockerfile"

    [ ! -f "${DOCKER_CONFIG}" ] && {
        printf "${CON_LRED}Error${CON_RESET}: Docker file not found (%s)\n" "${DOCKER_CONFIG}"
        exit 1
    }

    [ -n "${DOCKER_NAME}" ] && {
        DOCKER_ARGS="${DOCKER_ARGS} --name ${DOCKER_NAME}"
    }

    printf "Generate Docker image...\n"
    printf "  Docker base image... (%s)\n" "${DOCKER_BASE}"
    printf "  Docker file... (%s)\n" "${DOCKER_CONFIG}"
    printf "  Docker repository... (%s)\n" "${DOCKER_REPO}"
    printf "  Docker tag name... (%s)\n" "${DOCKER_TAG}"
    docker build \
        --network=host \
        --file "${DOCKER_CONFIG}" \
        --no-cache \
        --build-arg "host_uid=$(id -u)" \
        --build-arg "host_gid=$(id -g)" \
        --build-arg "user_name=${DOCKER_USER}" \
        --build-arg "DOCKER_TAG=${DOCKER_TAG}" \
        --build-arg "BASE_IMAGE=${DOCKER_BASE}" \
        --tag "${DOCKER_REPO}:${DOCKER_TAG}" \
        .
    exit $?
else
    printf "Run Docker image... (%s)\n" "${DOCKER_REPO}"

    DOCKER_CONFIG="${WORK_DIR}/dockerfile.config"
    [ -f "${DOCKER_CONFIG}" ] && {
        printf "Read configuration file '%s'...\n" "${DOCKER_CONFIG}"
        parse_config "${DOCKER_CONFIG}"
    }

    [[ -f "${DOCKER_ENV}" ]] && {
        DOCKER_ARGS="${DOCKER_ARGS} --env-file ${DOCKER_ENV}"
    }

    ensure_macos_volumes

    docker run \
        --interactive \
        --tty \
        --rm \
        -v /tmp/.X11-unix:/tmp/.X11-unix \
        --network host \
        --privileged \
        -e "TERM=xterm-256color" \
        -e "LANG=C.UTF-8" \
        -e "DOCKER_TAG=${DOCKER_TAG}" \
        --hostname ${DOCKER_HOST} \
        ${DOCKER_ARGS} \
        "${DOCKER_REPO}:${DOCKER_TAG}"
    exit $?
fi


# partuuid.bbclass - Generate and cache UUIDs for partition references
#
# Exposes:
#   EDGE_BOOT_PARTUUID
#   EDGE_ROOT_PARTUUID
#
#   do_generate_partuuids: writes the chosen values to:
#     - ${WORKDIR}/partuuids.conf
#     - ${DEPLOY_DIR_IMAGE}/partuuids-${IMAGE_BASENAME}.conf
#
# Behavior:
#   - Read PARTUUIDs from a cache file if it exists
#   - Otherwise, generate fresh UUIDv4 values, write cache, and use them
#
PARTUUID_CACHE_DIR  ?= "${TOPDIR}/uuid-cache"
PARTUUID_CACHE_FILE ?= "${PARTUUID_CACHE_DIR}/partuuids.conf"

# Name of the audit file dropped next to images:
PARTUUIDS_DEPLOY_NAME ?= "partuuids-${IMAGE_BASENAME}.conf"

python __anonymous() {
    import os, uuid

    cache_dir  = d.getVar('PARTUUID_CACHE_DIR')
    cache_file = d.getVar('PARTUUID_CACHE_FILE')

    # Ensure cache dir exists (safe at parse-time)
    if not os.path.isdir(cache_dir):
        os.makedirs(cache_dir, exist_ok=True)

    boot_uuid = None
    root_uuid = None

    # try to read from the cache
    if os.path.exists(cache_file):
        try:
            with open(cache_file, 'r') as f:
                for line in f:
                    if line.startswith('EDGE_BOOT_PARTUUID='):
                        boot_uuid = line.split('=', 1)[1].strip()
                    elif line.startswith('EDGE_ROOT_PARTUUID='):
                        root_uuid = line.split('=', 1)[1].strip()
        except Exception as e:
            bb.warn(f"partuuids: failed to read cache: {e}")

    if not boot_uuid or not root_uuid:
        # generate if missing and update the cache
        boot_uuid = str(uuid.uuid4())
        root_uuid = str(uuid.uuid4())
        try:
            with open(cache_file, 'w') as f:
                f.write(f"EDGE_BOOT_PARTUUID={boot_uuid}\n")
                f.write(f"EDGE_ROOT_PARTUUID={root_uuid}\n")
            bb.note(f"partuuids: generated & cached boot={boot_uuid} root={root_uuid}")
        except Exception as e:
            bb.warn(f"partuuids: failed to write cache: {e}")

    # export the variables to datastore
    d.setVar('EDGE_BOOT_PARTUUID', boot_uuid)
    d.setVar('EDGE_ROOT_PARTUUID', root_uuid)

    # make the variables available to WIC as well
    wicvars = (d.getVar('WICVARS') or '')
    extra = ' EDGE_BOOT_PARTUUID EDGE_ROOT_PARTUUID'
    if extra not in wicvars:
        d.setVar('WICVARS', (wicvars + extra).strip())
}

python do_generate_partuuids() {
    import os

    boot_uuid = d.getVar('EDGE_BOOT_PARTUUID')
    root_uuid = d.getVar('EDGE_ROOT_PARTUUID')

    # helper file to be used if a shell task or external script wants to source the
    # values instead of relying on BitBake variable expansion
    work_conf = os.path.join(d.getVar('WORKDIR'), 'partuuids.conf')
    with open(work_conf, 'w') as f:
        f.write(f"EDGE_BOOT_PARTUUID={boot_uuid}\nEDGE_ROOT_PARTUUID={root_uuid}\n")

    # audit file to be used if post-build tooling/CI needs to read
    # the UUIDs from ${DEPLOY_DIR_IMAGE} without parsing BitBake logs
    deploy_dir = d.getVar('DEPLOY_DIR_IMAGE')
    deploy_name = d.getVar('PARTUUIDS_DEPLOY_NAME')
    if deploy_dir and deploy_name:
        os.makedirs(deploy_dir, exist_ok=True)
        with open(os.path.join(deploy_dir, deploy_name), 'w') as f:
            f.write(f"EDGE_BOOT_PARTUUID={boot_uuid}\nEDGE_ROOT_PARTUUID={root_uuid}\n")

    bb.note(f"partuuids: using boot={boot_uuid} root={root_uuid}")
}

addtask generate_partuuids before do_configure after do_patch

# re-run the task if these knobs change
do_generate_partuuids[vardeps] += "EDGE_BOOT_PARTUUID EDGE_ROOT_PARTUUID PARTUUID_CACHE_FILE PARTUUIDS_DEPLOY_NAME"

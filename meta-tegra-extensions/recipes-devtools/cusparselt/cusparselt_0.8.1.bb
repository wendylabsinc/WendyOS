SUMMARY = "NVIDIA cuSPARSELt - Lightweight Sparse Matrix Library"
DESCRIPTION = "cuSPARSELt is a high-performance CUDA library for sparse matrix-matrix multiplication"
HOMEPAGE = "https://developer.nvidia.com/cusparselt"
LICENSE = "Proprietary"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/Proprietary;md5=0557f9d92cf58f2ccdd50f62f8ac0b28"

# Download the local repo installer from NVIDIA
SRC_URI = "https://developer.download.nvidia.com/compute/cusparselt/${PV}/local_installers/cusparselt-local-tegra-repo-ubuntu2204-${PV}_${PV}-1_arm64.deb;name=repo"
SRC_URI[repo.sha256sum] = "c3445239a57331eedcd57cc760d3220983e0fbe7458cc12811bb2e8fa7fb60ad"

COMPATIBLE_MACHINE = "(tegra)"
PACKAGE_ARCH = "${TEGRA_PKGARCH}"

DEPENDS = "cuda-cudart"

S = "${WORKDIR}"

# The outer deb is a repo installer containing the actual package debs
do_unpack[depends] += "xz-native:do_populate_sysroot"

python do_unpack:append() {
    import subprocess
    import glob
    import os

    workdir = d.getVar('WORKDIR')

    # The outer deb extracts to var/cusparselt-local-tegra-repo-*/
    # which contains the actual .deb packages
    repo_dirs = glob.glob(os.path.join(workdir, 'var', 'cusparselt-local-tegra-repo-*'))
    if not repo_dirs:
        bb.fatal("Could not find cuSPARSELt local repo directory")

    repo_dir = repo_dirs[0]

    # Find and extract the CUDA 12 library deb
    lib_debs = glob.glob(os.path.join(repo_dir, 'libcusparselt0-cuda-12_*.deb'))
    if not lib_debs:
        bb.fatal("Could not find libcusparselt0-cuda-12 deb package")

    lib_deb = lib_debs[0]

    # Extract the library deb
    extract_dir = os.path.join(workdir, 'cusparselt')
    os.makedirs(extract_dir, exist_ok=True)

    # Use dpkg-deb style extraction: ar x, then unxz/untar
    subprocess.check_call(['ar', 'x', lib_deb], cwd=extract_dir)

    # Extract data.tar.xz (or data.tar.zst)
    data_tar = None
    for ext in ['data.tar.xz', 'data.tar.zst', 'data.tar.gz']:
        candidate = os.path.join(extract_dir, ext)
        if os.path.exists(candidate):
            data_tar = candidate
            break

    if data_tar:
        subprocess.check_call(['tar', 'xf', data_tar], cwd=extract_dir)

    # Also extract dev package if present
    dev_debs = glob.glob(os.path.join(repo_dir, 'libcusparselt0-dev-cuda-12_*.deb'))
    if dev_debs:
        dev_deb = dev_debs[0]
        dev_dir = os.path.join(workdir, 'cusparselt-dev')
        os.makedirs(dev_dir, exist_ok=True)
        subprocess.check_call(['ar', 'x', dev_deb], cwd=dev_dir)
        for ext in ['data.tar.xz', 'data.tar.zst', 'data.tar.gz']:
            candidate = os.path.join(dev_dir, ext)
            if os.path.exists(candidate):
                subprocess.check_call(['tar', 'xf', candidate], cwd=dev_dir)
                break
}

do_configure() {
    :
}

do_compile() {
    :
}

do_install() {
    install -d ${D}${libdir}

    # Install runtime libraries from the extracted deb
    # Libraries are in usr/lib/aarch64-linux-gnu/libcusparseLt/12/
    if [ -d ${WORKDIR}/cusparselt/usr/lib/aarch64-linux-gnu/libcusparseLt/12 ]; then
        for lib in ${WORKDIR}/cusparselt/usr/lib/aarch64-linux-gnu/libcusparseLt/12/*.so*; do
            if [ -f "$lib" ]; then
                install -m 0755 "$lib" ${D}${libdir}/
            fi
        done
    fi

    # Create standard symlinks if they don't exist
    cd ${D}${libdir}
    if [ -f libcusparseLt.so.0.8.1.1 ] && [ ! -e libcusparseLt.so.0 ]; then
        ln -sf libcusparseLt.so.0.8.1.1 libcusparseLt.so.0
    fi
    if [ -e libcusparseLt.so.0 ] && [ ! -e libcusparseLt.so ]; then
        ln -sf libcusparseLt.so.0 libcusparseLt.so
    fi

    # Install headers from dev package
    if [ -d ${WORKDIR}/cusparselt-dev/usr/include ]; then
        install -d ${D}${includedir}
        install -m 0644 ${WORKDIR}/cusparselt-dev/usr/include/*.h ${D}${includedir}/ 2>/dev/null || true
    fi
}

PACKAGES = "${PN} ${PN}-dev"

FILES:${PN} = "${libdir}/libcusparseLt.so.*"
FILES:${PN}-dev = "${includedir} ${libdir}/libcusparseLt.so"

RDEPENDS:${PN} = "cuda-cudart"

INSANE_SKIP:${PN} = "ldflags already-stripped"
INHIBIT_PACKAGE_STRIP = "1"
INHIBIT_PACKAGE_DEBUG_SPLIT = "1"
INHIBIT_SYSROOT_STRIP = "1"

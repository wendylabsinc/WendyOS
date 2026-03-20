SUMMARY = "NVIDIA Container Support Packages"
DESCRIPTION = "Ensures all NVIDIA libraries and tools referenced in l4t.csv are installed"

PR = "r0"
PACKAGE_ARCH = "${MACHINE_ARCH}"

inherit packagegroup

# This package group is designed for L4T ${L4T_VERSION}
# CUDA ${CUDA_VERSION}, cuDNN ${CUDNN_VERSION}, TensorRT ${TENSORRT_VERSION}
# Version pinning is controlled in conf/distro/include/l4t-r36-4-4.conf

# Based on l4t.csv analysis, these are the required NVIDIA packages:
#
# CUDA Runtime and Math Libraries (from CUDA toolkit):
# - cuda-cudart (libcudart)
# - cuda-cublas (libcublas, libcublasLt)
# - cuda-cusparse (libcusparse)
# - cuda-cusolver (libcusolver, libcusolverMg)
# - cuda-curand (libcurand)
# - cuda-cufft (libcufft)
# - cuda-nvrtc (libnvrtc, libnvrtc-builtins)
# - cuda-nvjitlink (libnvJitLink)
# - cuda-nvtx (libnvToolsExt)
# - cuda-cupti (libcupti)
# - cuda-nvjpeg (libnvjpeg)
# - cuda-npp (libnpp* - image processing)
# - cuda-cudla (libcudla - DLA runtime)
# - cuda-cufile (libcufile - GPUDirect Storage)
#
# cuDNN (Deep Learning primitives):
# - libcudnn9
#
# cuSPARSELt (Lightweight sparse operations):
# - libcusparselt0
#
# Note: cuDSS (Direct sparse linear solver - required by PyTorch 2.8+) is NOT
# included in the base OS. Install it in containers via pip when needed:
#   pip install nvidia-cudss-cu12
#
# TensorRT (Inference optimization):
# - libnvinfer10
# - libnvinfer-plugin10
# - libnvonnxparser10
#
# L4T Multimedia and Tegra Libraries:
# - nvidia-l4t-multimedia (nvbufsurface, nvbufsurftransform, nvdsbufferpool, nvbuf_fdmap)
# - nvidia-l4t-nvsci (nvscibuf, nvscicommon, nvscisync, nvscistream, nvscievent, nvsciipc)
# - nvidia-l4t-camera (libnvv4l2, libtegrav4l2, libv4l2_nvvideocodec)
# - nvidia-l4t-multimedia-utils (libnvmm, libnvmm_utils, libnvmmlite*)
# - nvidia-l4t-graphics (EGL, OpenGL - libEGL_nvidia, libGLESv2_nvidia, libnvidia-eglcore, libnvidia-glcore)
# - nvidia-l4t-cuda (libcuda driver)
#
# Container Runtime:
# - nvidia-container-toolkit (nvidia-ctk for CDI generation)
# - nvidia-container-runtime

# Core packages required for l4t.csv container support (always included)
RDEPENDS:${PN} = " \
    nvidia-container-config \
    nvidia-container-toolkit \
    libnvidia-container \
    nerdctl \
    cuda-toolkit \
    cuda-cudart \
    cuda-libraries \
    cuda-nvrtc \
    cuda-nvtx \
    cuda-cupti \
    tegra-libraries-core \
    tegra-libraries-cuda \
    tegra-libraries-multimedia \
    tegra-libraries-multimedia-utils \
    tegra-libraries-multimedia-v4l \
    tegra-libraries-nvsci \
    tegra-libraries-camera \
    tegra-libraries-eglcore \
    tegra-libraries-glescore \
    cudnn \
    cusparselt \
    tensorrt-core \
    tensorrt-plugins \
    libcufile \
    "

# DeepStream-specific packages (only when WENDYOS_DEEPSTREAM=1)
# These provide libraries needed by DeepStream GStreamer plugins
WENDYOS_DEEPSTREAM ?= "0"
RDEPENDS:${PN} += "${@bb.utils.contains('WENDYOS_DEEPSTREAM', '1', ' \
    tegra-libraries-multimedia-ds \
    tegra-libraries-nvdsseimeta \
    libgstnvcustomhelper \
    yaml-cpp-070 \
    tensorrt-trtexec-prebuilt \
    ', '', d)}"

# Note: cuda-libraries likely includes:
#  - cuBLAS (cublas, cublasLt)
#  - cuSPARSE (cusparse)
#  - cuSOLVER (cusolver, cusolverMg)
#  - cuRAND (curand)
#  - cuFFT (cufft)
#  - NPP (npp* image processing)
#
# cuSPARSELt (lightweight sparse ops) is packaged separately via the cusparselt
# recipe in meta-wendyos-jetson/recipes-devtools/cusparselt/

# Note: TensorRT now included since opengl is enabled in distro config

# Optional packages for additional functionality
# Uncomment as needed:
#
# Additional CUDA tools:
# RDEPENDS:${PN} += "cuda-samples"           # CUDA sample programs
# RDEPENDS:${PN} += "cuda-gdb"               # CUDA debugger
# RDEPENDS:${PN} += "tegra-cuda-utils"       # Tegra CUDA utilities
#
# TensorRT extras:
# RDEPENDS:${PN} += "tensorrt-trtexec"       # TensorRT execution utility
# RDEPENDS:${PN} += "tensorrt-samples"       # TensorRT samples
#
# cuDNN samples:
# RDEPENDS:${PN} += "cudnn-samples"          # cuDNN sample programs
#
# Python support:
# RDEPENDS:${PN} += "python3-tensorrt"       # Python TensorRT bindings
# RDEPENDS:${PN} += "python3-pycuda"         # Python CUDA bindings
#
# Note: Most CUDA math libraries (cublas, cusparse, cusolver, etc.) are included
# in cuda-libraries and cuda-toolkit packages

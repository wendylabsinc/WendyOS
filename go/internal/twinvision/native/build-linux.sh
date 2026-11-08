#!/bin/sh
set -eu
cd /src
. ./opencv.lock
curl --fail --location --retry 3 "https://github.com/opencv/opencv/archive/refs/tags/$OPENCV_VERSION.tar.gz" -o /tmp/opencv.tar.gz
printf '%s  /tmp/opencv.tar.gz\n' "$OPENCV_SHA256" | sha256sum -c -
tar -xzf /tmp/opencv.tar.gz -C /tmp
cmake -S "/tmp/opencv-$OPENCV_VERSION" -B /tmp/opencv-build -G Ninja \
 -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/opencv \
 -DBUILD_LIST=core,imgproc,features2d,calib3d,flann -DBUILD_SHARED_LIBS=OFF \
 -DBUILD_TESTS=OFF -DBUILD_PERF_TESTS=OFF -DBUILD_EXAMPLES=OFF -DBUILD_opencv_apps=OFF \
 -DBUILD_JAVA=OFF -DBUILD_opencv_python2=OFF -DBUILD_opencv_python3=OFF \
 -DWITH_ITT=OFF -DWITH_IPP=OFF -DWITH_OPENCL=OFF -DWITH_OPENCLAMDBLAS=OFF \
 -DWITH_LAPACK=OFF -DWITH_EIGEN=OFF -DWITH_ADE=OFF -DWITH_PROTOBUF=OFF \
 -DWITH_FFMPEG=OFF -DWITH_GSTREAMER=OFF -DWITH_AVFOUNDATION=OFF \
 -DWITH_JPEG=OFF -DWITH_PNG=OFF -DWITH_TIFF=OFF -DWITH_OPENEXR=OFF -DWITH_WEBP=OFF \
 -DCV_ENABLE_INTRINSICS=ON -DOPENCV_ENABLE_NONFREE=OFF
cmake --build /tmp/opencv-build --parallel 8
cmake --install /tmp/opencv-build

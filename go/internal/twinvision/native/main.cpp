// Private bounded binary protocol with the Go service. No client runs this worker.
#include "SiteLocalization.h"
#include <iostream>
#include <vector>
#include <opencv2/core.hpp>

template<class T> void read(T &v) {
 if (!std::cin.read(reinterpret_cast<char*>(&v), sizeof(v))) throw 1;
}
template<class T> void write(const T &v) {
 std::cout.write(reinterpret_cast<const char*>(&v), sizeof(v));
}
int main() {
 try {
  // Replay decisions must not depend on a previous request's OpenCV RNG state.
  cv::setNumThreads(1); cv::setRNGSeed(0);
  int32_t version, width, height, count, mode, provenance;
  read(version); read(width); read(height); read(count); read(mode); read(provenance);
  if(version!=1 || width<32 || height<32 || width>8192 || height>8192 ||
     int64_t(width)*height>32000000 || count<120 || count>WS_MAX_LANDMARKS ||
     mode<0 || mode>1 || provenance<0 || provenance>2) return 2;
  double lens[9]; for(auto &v:lens) read(v);
  std::vector<float> landmarks(size_t(count)*WS_LANDMARK_FLOATS);
  std::vector<uint8_t> pixels(size_t(width)*height);
  if(!std::cin.read(reinterpret_cast<char*>(landmarks.data()), landmarks.size()*sizeof(float)) ||
     !std::cin.read(reinterpret_cast<char*>(pixels.data()), pixels.size())) return 2;
  void *map=WSMapCreate();
  if(!map || WSMapImport(map,landmarks.data(),count)!=count) return 3;
  WSLocalization result{}; int32_t source=0;
  if(mode==1 || provenance==1) {
   result=provenance==2 ? WSLocalizeWithAssumedLens(map,pixels.data(),width,height,lens)
                        : WSLocalize(map,pixels.data(),width,height,lens);
   if(provenance==2) result.fitted_lens=2;
  } else {
   source=1; result=WSLocalize(map,pixels.data(),width,height,nullptr);
   if(!result.success && provenance==2) {
    source=2; result=WSLocalizeWithAssumedLens(map,pixels.data(),width,height,lens);
    result.fitted_lens=2;
   }
   if(!result.success) { source=3; result=WSLocalizeEstimated(map,pixels.data(),width,height); }
  }
  WSMapDestroy(map);
  write(source); write(result.success); write(result.reason); write(result.matches);
  write(result.inliers); write(result.fitted_lens);
  write(result.fx); write(result.fy); write(result.cx); write(result.cy);
  for(auto v:result.distortion) write(v);
  write(result.rms); for(auto v:result.world_from_camera) write(v);
  return std::cout ? 0 : 4;
 } catch(...) { return 5; }
}

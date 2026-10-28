#include "../Shared/SiteLocalization.h"
#include <opencv2/core.hpp>
#include <opencv2/imgproc.hpp>
#include <opencv2/features2d.hpp>
#include <opencv2/calib3d.hpp>
#include <cmath>
#include <vector>
#include <set>
#include <tuple>
#include <algorithm>

namespace {
struct Map {
 std::vector<cv::Point3f> points;
 cv::Mat descriptors;
 cv::Ptr<cv::SIFT> sift=cv::SIFT::create(800);
 uint64_t observations=0;
};
// A bounded reservoir keeps sampling after the map fills. Otherwise the first
// part of a room scan consumes every slot and later viewpoints are discarded.
uint64_t reservoirIndex(uint64_t count){
 uint64_t value=count+0x9e3779b97f4a7c15ULL;
 value=(value^(value>>30))*0xbf58476d1ce4e5b9ULL;
 value=(value^(value>>27))*0x94d049bb133111ebULL;
 return (value^(value>>31))%count;
}
// RootSIFT compares normalized gradient histograms using Hellinger distance.
// https://www.robots.ox.ac.uk/~vgg/publications/2012/Arandjelovic12/
cv::Mat rootDescriptors(const cv::Mat& input){
 cv::Mat result=input.clone();
 for(int i=0;i<result.rows;++i){float*row=result.ptr<float>(i);double sum=0;
  for(int j=0;j<result.cols;++j)sum+=row[j];
  for(int j=0;j<result.cols;++j)row[j]=std::sqrt(row[j]/std::max(sum,1e-12));
 }return result;
}
bool imageSize(int w,int h){return w>=32&&h>=32&&w<=8192&&h<=8192&&int64_t(w)*h<=32000000;}
bool finiteValues(const double *p,int count){if(!p)return false;for(int i=0;i<count;++i)if(!std::isfinite(p[i]))return false;return true;}
std::tuple<int,int,int> voxel(const cv::Point3f &p){return {int(std::floor(p.x*50)),int(std::floor(p.y*50)),int(std::floor(p.z*50))};}
bool nonplanar(const std::vector<cv::Point3f> &p){
 cv::Mat a(int(p.size()),3,CV_64F);cv::Vec3d mean(0,0,0);for(auto&v:p)mean+=cv::Vec3d(v.x,v.y,v.z);mean/=double(p.size());
 for(int i=0;i<int(p.size());++i){a.at<double>(i,0)=p[i].x-mean[0];a.at<double>(i,1)=p[i].y-mean[1];a.at<double>(i,2)=p[i].z-mean[2];}
 cv::Mat s;cv::SVD::compute(a,s);return s.at<double>(2)>0.04*s.at<double>(0)&&s.at<double>(2)/std::sqrt(double(p.size()))>0.06;
}
double reprojection(const std::vector<cv::Point3f>&xyz,const std::vector<cv::Point2f>&uv,const cv::Mat&K,const cv::Mat&D,const cv::Mat&r,const cv::Mat&t,double maximumError=8){
 std::vector<cv::Point2f> projected;cv::projectPoints(xyz,r,t,K,D,projected);double sum=0;
 for(size_t i=0;i<uv.size();++i){double e=cv::norm(projected[i]-uv[i]);if(!std::isfinite(e)||e>maximumError)return 1e9;sum+=e*e;}
 return std::sqrt(sum/uv.size());
}
bool validLens(const cv::Mat&K,const cv::Mat&D,int w,int h){
 double fx=K.at<double>(0,0),fy=K.at<double>(1,1),cx=K.at<double>(0,2),cy=K.at<double>(1,2);
 if(!cv::checkRange(K)||!cv::checkRange(D)||fx<.3*w||fx>5*w||fy<.3*h||fy>5*w||cx<.15*w||cx>.85*w||cy<.15*h||cy>.85*h)return false;
 for(int i=0;i<5;++i)if(std::abs(D.at<double>(i))>2)return false;
 for(int y=0;y<=8;++y)for(int x=0;x<=8;++x){double a=(x*w/8.0-cx)/fx,b=(y*h/8.0-cy)/fy,r=a*a+b*b;
  double radial=1+D.at<double>(0)*r+D.at<double>(1)*r*r+D.at<double>(4)*r*r*r;
  double slope=1+3*D.at<double>(0)*r+5*D.at<double>(1)*r*r+7*D.at<double>(4)*r*r*r;
  if(radial<.3||slope<.2)return false;
 }return true;
}
}
extern "C" void *WSMapCreate(){try{return new Map();}catch(...){return nullptr;}}
extern "C" void WSMapDestroy(void*p){delete static_cast<Map*>(p);}
extern "C" int32_t WSMapCount(void*p){return p?int(static_cast<Map*>(p)->points.size()):0;}
extern "C" int32_t WSMapAdd(void*p,const uint8_t*gray,int32_t w,int32_t h,const float*depth,const uint8_t*confidence,int32_t dw,int32_t dh,const double*k,const double*T){
 if(!p||!gray||!depth||!confidence||!imageSize(w,h)||dw<8||dh<8||dw>1024||dh>1024||!finiteValues(k,4)||!finiteValues(T,16)||k[0]<=0||k[1]<=0)return -1;
 try{
  auto&m=*static_cast<Map*>(p);
  std::vector<cv::KeyPoint> keys;cv::Mat desc;m.sift->detectAndCompute(cv::Mat(h,w,CV_8U,const_cast<uint8_t*>(gray)),cv::noArray(),keys,desc);
  std::multiset<std::tuple<int,int,int>> seen;for(auto&v:m.points)seen.insert(voxel(v));
  for(size_t i=0;i<keys.size();++i){
   double x=keys[i].pt.x,y=keys[i].pt.y;int u=int(x*dw/w),v=int(y*dh/h);
   if(u<1||v<1||u>=dw-1||v>=dh-1||confidence[v*dw+u]<2)continue;
   float z=depth[v*dw+u];if(!std::isfinite(z)||z<.25||z>6)continue;
   bool smooth=true;for(int yy=v-1;yy<=v+1;++yy)for(int xx=u-1;xx<=u+1;++xx){float d=depth[yy*dw+xx];if(!std::isfinite(d)||std::abs(d-z)>.05)smooth=false;}
   if(!smooth)continue;double cx=(x-k[2])*z/k[0],cy=(y-k[3])*z/k[1];
   cv::Point3f world(T[0]*cx+T[4]*cy+T[8]*z+T[12],T[1]*cx+T[5]*cy+T[9]*z+T[13],T[2]*cx+T[6]*cy+T[10]*z+T[14]);
   if(!std::isfinite(world.x)||!std::isfinite(world.y)||!std::isfinite(world.z)||cv::norm(world)>1000||seen.count(voxel(world)))continue;
   ++m.observations;
   if(m.points.size()<WS_MAX_LANDMARKS){
    m.points.push_back(world);m.descriptors.push_back(desc.row(int(i)));
   }else{
    auto slot=reservoirIndex(m.observations);
    if(slot>=WS_MAX_LANDMARKS)continue;
    seen.erase(seen.find(voxel(m.points[slot])));
    m.points[slot]=world;desc.row(int(i)).copyTo(m.descriptors.row(int(slot)));
   }
   seen.insert(voxel(world));
  }return int(m.points.size());
 }catch(...){return -1;}
}
extern "C" int32_t WSMapExport(void*p,float*out,int32_t capacity){
 if(!p||!out)return -1;auto&m=*static_cast<Map*>(p);if(capacity<int(m.points.size()))return -1;
 for(size_t i=0;i<m.points.size();++i){float*row=out+i*WS_LANDMARK_FLOATS;row[0]=m.points[i].x;row[1]=m.points[i].y;row[2]=m.points[i].z;std::copy(m.descriptors.ptr<float>(int(i)),m.descriptors.ptr<float>(int(i))+128,row+3);}return int(m.points.size());
}
extern "C" int32_t WSMapImport(void*p,const float*data,int32_t count){
 if(!p||!data||count<0||count>WS_MAX_LANDMARKS)return -1;
 try{Map next;for(int i=0;i<count;++i){auto*row=data+i*WS_LANDMARK_FLOATS;
  for(int j=0;j<WS_LANDMARK_FLOATS;++j)if(!std::isfinite(row[j])||(j<3?std::abs(row[j])>1000:(row[j]<0||row[j]>512)))return -1;
  next.points.emplace_back(row[0],row[1],row[2]);next.descriptors.push_back(cv::Mat(1,128,CV_32F,const_cast<float*>(row+3)).clone());
 }next.observations=uint64_t(count);*static_cast<Map*>(p)=std::move(next);return count;}catch(...){return -1;}
}
static WSLocalization solveCameraFit(const double*xyz,const double*uv,int32_t n,int32_t w,int32_t h,const double*known,bool estimated,bool refineFocal=false){
 WSLocalization out={};out.reason=1;out.matches=n;
 const int minimum=estimated?WS_MIN_ESTIMATED_INLIERS:(known?WS_MIN_POSE_INLIERS:WS_MIN_LENS_INLIERS);
 if(n<minimum||n>1800||!imageSize(w,h)||!finiteValues(xyz,n*3)||!finiteValues(uv,n*2)||(known&&!finiteValues(known,9)))return out;
 try{
  std::vector<cv::Point3f> points;std::vector<cv::Point2f> pixels;
  for(int i=0;i<n;++i){if(std::abs(xyz[i*3])>1000||std::abs(xyz[i*3+1])>1000||std::abs(xyz[i*3+2])>1000||uv[i*2]<0||uv[i*2]>=w||uv[i*2+1]<0||uv[i*2+1]>=h)return out;points.emplace_back(xyz[i*3],xyz[i*3+1],xyz[i*3+2]);pixels.emplace_back(uv[i*2],uv[i*2+1]);}
  cv::Mat bestK,bestR,bestT,bestMask;int best=0;double bestError=1e9;
  cv::Mat distortion=cv::Mat::zeros(5,1,CV_64F);if(known)for(int i=0;i<5;++i)distortion.at<double>(i)=known[i+4];
  std::vector<double> guesses=known?std::vector<double>{known[0]}:std::vector<double>{.45*w,.6*w,.8*w,1.0*w,1.4*w,2.0*w};
  for(double f:guesses){cv::Mat K=(cv::Mat_<double>(3,3)<<f,0,known?known[2]:w/2.0,0,known?known[1]:f,known?known[3]:h/2.0,0,0,1),r,t,mask;
   bool ok=cv::solvePnPRansac(points,pixels,K,distortion,r,t,false,estimated?3000:1500,(known&&!estimated)?3.0:6.0,.999,mask,cv::SOLVEPNP_EPNP);
   if(ok&&mask.rows>=best&&mask.rows>0){
    std::vector<cv::Point3f> supported;std::vector<cv::Point2f> observed;
    for(int i=0;i<mask.rows;++i){int index=mask.at<int>(i);supported.push_back(points[index]);observed.push_back(pixels[index]);}
    double error=reprojection(supported,observed,K,distortion,r,t);
    // Sparse sets often give several focal guesses the same consensus count.
    // Choose the better geometric fit instead of depending on guess order.
    if(mask.rows>best||error<bestError){best=mask.rows;bestError=error;bestK=K;bestR=r;bestT=t;bestMask=mask;}
   }
  }
  out.reason=2;out.inliers=best;if(best<minimum||best<n*(estimated?.25:.4))return out;
  std::vector<cv::Point3f> consensus;
  std::vector<cv::Point2f> allUV;
  for(int i=0;i<best;++i){int index=bestMask.at<int>(i);consensus.push_back(points[index]);allUV.push_back(pixels[index]);}
  out.reason=3;std::vector<cv::Point2f> hull;cv::convexHull(allUV,hull);auto box=cv::boundingRect(allUV);
  const bool poseOnly=known||estimated;
  if(std::abs(cv::contourArea(hull))<(poseOnly?.04:.20)*w*h||box.width<(poseOnly?.20:.55)*w||box.height<(poseOnly?.20:.55)*h)return out;
  out.reason=7;if(!poseOnly&&!nonplanar(consensus))return out;
  cv::Mat K,D,r,t;
  double validationSum=0;int validationCount=0;
  // Small sets must survive four independent train/validation splits. Every
  // correspondence is withheld once; neither duplicated observations nor a
  // low training residual can stand in for geometric evidence.
  const int folds=best<60?4:1;
  for(int fold=0;fold<folds;++fold){
   std::vector<cv::Point3f> train,hold;std::vector<cv::Point2f> trainUV,holdUV;
   for(int i=0;i<best;++i){if(i%4==fold){hold.push_back(consensus[i]);holdUV.push_back(allUV[i]);}else{train.push_back(consensus[i]);trainUV.push_back(allUV[i]);}}
   out.reason=refineFocal?8:7;if((refineFocal||(!known&&!estimated))&&!nonplanar(train))return out;
   cv::Mat fitK=bestK.clone(),fitD=distortion.clone(),fitR=bestR.clone(),fitT=bestT.clone();
   out.reason=4;
   if((known||estimated)&&!refineFocal){cv::solvePnPRefineLM(train,trainUV,fitK,fitD,fitR,fitT);}
   else{
    std::vector<cv::Mat> rotations,translations;cv::Mat sdK,sdPose,viewError;
    int flags=cv::CALIB_USE_INTRINSIC_GUESS|cv::CALIB_FIX_ASPECT_RATIO|cv::CALIB_ZERO_TANGENT_DIST|cv::CALIB_FIX_K3;
    if(refineFocal)flags|=cv::CALIB_FIX_PRINCIPAL_POINT|cv::CALIB_FIX_K1|cv::CALIB_FIX_K2;
    cv::calibrateCamera(std::vector<std::vector<cv::Point3f>>{train},std::vector<std::vector<cv::Point2f>>{trainUV},cv::Size(w,h),fitK,fitD,rotations,translations,sdK,sdPose,viewError,flags,cv::TermCriteria(cv::TermCriteria::COUNT|cv::TermCriteria::EPS,100,1e-9));
    if(rotations.empty()||!cv::checkRange(sdK)||sdK.at<double>(1)>fitK.at<double>(1,1)*(estimated?.30:.08)||sdK.at<double>(2)>w*.05||sdK.at<double>(3)>h*.05)return out;
    fitR=rotations[0];fitT=translations[0];
   }
   if(!validLens(fitK,fitD,w,h))return out;
   double training=reprojection(train,trainUV,fitK,fitD,fitR,fitT,estimated?16:8),validation=reprojection(hold,holdUV,fitK,fitD,fitR,fitT,estimated?16:8);
   out.reason=5;out.rms=validation;if(training>(estimated?5:2.5)||validation>(estimated?5:2.5))return out;
   validationSum+=validation*validation*hold.size();validationCount+=int(hold.size());
   if(fold==0){K=fitK;D=fitD;r=fitR;t=fitT;}
   else{
    cv::Mat firstR,nextR;cv::Rodrigues(r,firstR);cv::Rodrigues(fitR,nextR);
    cv::Mat delta=nextR*firstR.t();double cosine=(cv::trace(delta)[0]-1)*.5;
    double angle=std::acos(std::clamp(cosine,-1.,1.));
    cv::Mat firstPosition=-firstR.t()*t,nextPosition=-nextR.t()*fitT;
    if(angle>(estimated?10:3)*CV_PI/180||cv::norm(firstPosition-nextPosition)>(estimated?.30:.08)||
       std::abs(fitK.at<double>(0,0)/K.at<double>(0,0)-1)>(estimated?.20:.05)||
       std::abs(fitK.at<double>(0,2)-K.at<double>(0,2))>.03*w||
       std::abs(fitK.at<double>(1,2)-K.at<double>(1,2))>.03*h)return out;
    std::vector<cv::Point2f> edge{{0,0},{float(w)/2,0},{float(w),0},{float(w),float(h)/2},{float(w),float(h)},{float(w)/2,float(h)},{0,float(h)},{0,float(h)/2}},firstRays,nextRays;
    cv::undistortPoints(edge,firstRays,K,D);cv::undistortPoints(edge,nextRays,fitK,fitD);
    for(size_t j=0;j<edge.size();++j){cv::Vec3d a(firstRays[j].x,firstRays[j].y,1),b(nextRays[j].x,nextRays[j].y,1);
     double cosine=a.dot(b)/(cv::norm(a)*cv::norm(b));
     if(!std::isfinite(cosine)||std::acos(std::clamp(cosine,-1.,1.))>(estimated?10:3)*CV_PI/180)return out;
    }
   }
  }
  out.rms=std::sqrt(validationSum/validationCount);
  out.fitted_lens=known?0:(estimated?2:1);
  cv::Mat R;cv::Rodrigues(r,R);cv::Mat worldR=R.t(),position=-worldR*t;
  for(auto&p:consensus){cv::Mat v=(cv::Mat_<double>(3,1)<<p.x,p.y,p.z);cv::Mat q=R*v+t;if(q.at<double>(2)<=.1)return out;}
  if(cv::norm(position)>1000)return out;
  for(int col=0;col<3;++col)for(int row=0;row<3;++row)out.world_from_camera[col*4+row]=worldR.at<double>(row,col);
  for(int row=0;row<3;++row)out.world_from_camera[12+row]=position.at<double>(row);out.world_from_camera[15]=1;
  out.fx=K.at<double>(0,0);out.fy=K.at<double>(1,1);out.cx=K.at<double>(0,2);out.cy=K.at<double>(1,2);
  for(int i=0;i<5;++i)out.distortion[i]=D.at<double>(i);out.success=1;out.reason=0;return out;
 }catch(...){out.reason=6;return out;}
}
static WSLocalization solveCamera(const double*xyz,const double*uv,int32_t n,int32_t w,int32_t h,const double*known,bool estimated){
 if(!estimated||known)return solveCameraFit(xyz,uv,n,w,h,known,estimated);
 try{
 // Validate every assumed focal length independently. Choosing a RANSAC
 // winner first can discard a valid solution, or hide focal/depth ambiguity.
 WSLocalization best={};best.reason=1;best.matches=n;
 std::vector<WSLocalization> candidates;
 for(double factor:{.35,.45,.6,.8,1.0,1.4,2.0}){
  double lens[]={factor*w,factor*w,w/2.,h/2.,0,0,0,0,0};
  auto fit=solveCameraFit(xyz,uv,n,w,h,lens,true);
  if(fit.success)candidates.push_back(fit);
  else if(fit.inliers>best.inliers||(fit.inliers==best.inliers&&fit.reason>best.reason))best=fit;
  // With real depth variation, solve the focal length continuously rather
  // than quantizing distance to the seed grid. This remains an estimated
  // pinhole lens, with principal point and distortion explicitly assumed.
  auto refined=solveCameraFit(xyz,uv,n,w,h,lens,true,true);
  if(refined.success)candidates.push_back(refined);
 }
 if(candidates.empty())return best;
 best=candidates.front();
 for(auto&fit:candidates)if(fit.inliers>best.inliers||(fit.inliers==best.inliers&&fit.rms<best.rms))best=fit;
 for(auto&fit:candidates){
  if(fit.inliers<std::ceil(best.inliers*.9)||fit.rms>std::max(best.rms*1.5,best.rms+.5))continue;
  double distance=0,trace=0;
  for(int row=0;row<3;++row){
   double delta=fit.world_from_camera[12+row]-best.world_from_camera[12+row];distance+=delta*delta;
   for(int col=0;col<3;++col)trace+=fit.world_from_camera[col*4+row]*best.world_from_camera[col*4+row];
  }
  double angle=std::acos(std::clamp((trace-1)*.5,-1.,1.));
  if(std::sqrt(distance)>.30||angle>10*CV_PI/180){best.success=0;best.reason=8;return best;}
 }
 best.fitted_lens=2;
 return best;
 }catch(...){WSLocalization failure={};failure.reason=6;failure.matches=n;return failure;}
}
static WSLocalization localize(void*p,const uint8_t*gray,int32_t w,int32_t h,const double*known,bool estimated){
 WSLocalization out={};out.reason=1;if(!p||!gray||!imageSize(w,h))return out;
 try{
  auto&m=*static_cast<Map*>(p);if(m.points.size()<120)return out;
  cv::Mat full(h,w,CV_8U,const_cast<uint8_t*>(gray)),image,K,D,mask;
  if(known){
   if(!finiteValues(known,9))return out;
   K=(cv::Mat_<double>(3,3)<<known[0],0,known[2],0,known[1],known[3],0,0,1);
   D=(cv::Mat_<double>(5,1)<<known[4],known[5],known[6],known[7],known[8]);
   if(!validLens(K,D,w,h))return out;
   // Match undistorted feature patches to the phone's room references. Using
   // distortion only during PnP is too late if warped patches did not match.
   cv::Mat rectified,valid(h,w,CV_8U,cv::Scalar(255));
   cv::undistort(full,rectified,K,D,K);full=rectified;
   cv::undistort(valid,mask,K,D,K);cv::threshold(mask,mask,254,255,cv::THRESH_BINARY);
   cv::erode(mask,mask,cv::Mat(),cv::Point(-1,-1),4);
  }
  double scale=std::min(1.,960./std::max(w,h));cv::resize(full,image,cv::Size(),scale,scale,cv::INTER_AREA);
  if(!mask.empty())cv::resize(mask,mask,image.size(),0,0,cv::INTER_NEAREST);
  std::vector<cv::KeyPoint> keys;cv::Mat descriptors;cv::SIFT::create(1800)->detectAndCompute(image,mask,keys,descriptors);if(keys.size()<size_t(estimated?WS_MIN_ESTIMATED_INLIERS:(known?WS_MIN_POSE_INLIERS:WS_MIN_LENS_INLIERS)))return out;
  cv::FlannBasedMatcher matcher;std::vector<std::vector<cv::DMatch>> matches;matcher.knnMatch(rootDescriptors(descriptors),rootDescriptors(m.descriptors),matches,8);
  std::vector<cv::DMatch> candidates;
  for(auto&pair:matches){
   if(pair.size()<2)continue;
   // Repeat observations of one physical feature must not be its own
   // ambiguity competitor. Compare against a spatially distinct alternative.
   size_t competitor=1;
   while(competitor<pair.size()&&cv::norm(m.points[pair[competitor].trainIdx]-m.points[pair[0].trainIdx])<.08)++competitor;
   if(competitor<pair.size()&&pair[0].distance<.75*pair[competitor].distance)candidates.push_back(pair[0]);
  }
  std::sort(candidates.begin(),candidates.end(),[](auto&a,auto&b){return a.distance<b.distance;});
  std::vector<double> xyz,uv;std::vector<cv::Point3f> distinctPoints;std::vector<cv::Point2f> distinctPixels;
  for(auto&match:candidates){
   auto&point=m.points[match.trainIdx];cv::Point2f pixel=keys[match.queryIdx].pt/float(scale);
   bool duplicate=false;
   for(size_t i=0;i<distinctPoints.size();++i)if(cv::norm(point-distinctPoints[i])<.03||cv::norm(pixel-distinctPixels[i])<2){duplicate=true;break;}
   if(duplicate)continue;
   distinctPoints.push_back(point);distinctPixels.push_back(pixel);
   xyz.insert(xyz.end(),{point.x,point.y,point.z});uv.insert(uv.end(),{pixel.x,pixel.y});
  }
  if(known&&!uv.empty()){
   // PnP and its held-out residuals use the original camera model. Convert the
   // rectified keypoints back to raw pixels, applying lens distortion once.
   std::vector<cv::Point3f> rays;std::vector<cv::Point2f> raw;
   for(size_t i=0;i<uv.size();i+=2)rays.emplace_back((uv[i]-known[2])/known[0],(uv[i+1]-known[3])/known[1],1);
   cv::projectPoints(rays,cv::Vec3d(0,0,0),cv::Vec3d(0,0,0),K,D,raw);
   std::vector<double> keptXYZ,keptUV;
   for(size_t i=0;i<raw.size();++i)if(raw[i].x>=0&&raw[i].x<w&&raw[i].y>=0&&raw[i].y<h){
    keptXYZ.insert(keptXYZ.end(),xyz.begin()+i*3,xyz.begin()+i*3+3);
    keptUV.insert(keptUV.end(),{raw[i].x,raw[i].y});
   }
   xyz.swap(keptXYZ);uv.swap(keptUV);
  }
  return solveCamera(xyz.data(),uv.data(),int(uv.size()/2),w,h,known,estimated);
 }catch(...){out.reason=6;return out;}
}

extern "C" WSLocalization WSSolveCamera(const double*xyz,const double*uv,int32_t n,int32_t w,int32_t h,const double*known){return solveCamera(xyz,uv,n,w,h,known,false);}
extern "C" WSLocalization WSSolveCameraEstimated(const double*xyz,const double*uv,int32_t n,int32_t w,int32_t h){return solveCamera(xyz,uv,n,w,h,nullptr,true);}
extern "C" WSLocalization WSLocalize(void*p,const uint8_t*gray,int32_t w,int32_t h,const double*known){return localize(p,gray,w,h,known,false);}
extern "C" WSLocalization WSLocalizeWithAssumedLens(void*p,const uint8_t*gray,int32_t w,int32_t h,const double*known){return localize(p,gray,w,h,known,true);}
extern "C" WSLocalization WSLocalizeEstimated(void*p,const uint8_t*gray,int32_t w,int32_t h){return localize(p,gray,w,h,nullptr,true);}

namespace {
struct ScreenLens {
 int width=0,height=0,detection=0;
 uint64_t revision=0;
 cv::Rect lastBox;
 std::vector<std::vector<cv::Point2f>> views;
};
std::vector<cv::Point3f> screenPattern(){
 std::vector<cv::Point3f> points;
 for(int y=0;y<9;++y)for(int x=0;x<6;++x)points.emplace_back(float(x),float(y),0);
 return points;
}
}
extern "C" void* WSScreenLensCreate(){try{return new ScreenLens();}catch(...){return nullptr;}}
extern "C" void WSScreenLensDestroy(void*p){delete static_cast<ScreenLens*>(p);}
extern "C" int32_t WSScreenLensAdd(void*p,const uint8_t*gray,int32_t w,int32_t h){
 if(!p||!gray||!imageSize(w,h))return -1;
 try{
  auto&s=*static_cast<ScreenLens*>(p);
  if(s.width&&(s.width!=w||s.height!=h))return -2;
  s.width=w;s.height=h;s.detection=0;s.lastBox=cv::Rect();
  std::vector<cv::Point2f> corners;
  if(!cv::findChessboardCornersSB(cv::Mat(h,w,CV_8U,const_cast<uint8_t*>(gray)),cv::Size(6,9),corners,cv::CALIB_CB_NORMALIZE_IMAGE))return int(s.views.size());
  auto box=cv::boundingRect(corners);
  s.lastBox=box;
  if(box.width<60||box.height<60||box.area()<.025*w*h){s.detection=1;return int(s.views.size());}
  if(box.x<4||box.y<4||box.x+box.width>w-4||box.y+box.height>h-4){s.detection=2;return int(s.views.size());}
  for(auto&prior:s.views){double sum=0,reversed=0;for(size_t j=0;j<corners.size();++j){double a=cv::norm(corners[j]-prior[j]),b=cv::norm(corners[j]-prior[corners.size()-1-j]);sum+=a*a;reversed+=b*b;}
   cv::Point2f a(0,0),b(0,0);for(size_t j=0;j<corners.size();++j){a+=corners[j];b+=prior[j];}a/=float(corners.size());b/=float(corners.size());
   double radiusA=0,radiusB=0;for(size_t j=0;j<corners.size();++j){radiusA+=cv::norm(corners[j]-a);radiusB+=cv::norm(prior[j]-b);}radiusA/=corners.size();radiusB/=corners.size();
   double shape=0;for(size_t j=0;j<corners.size();++j){size_t k=sum<=reversed?j:corners.size()-1-j;double distance=cv::norm((corners[j]-a)/float(radiusA)-(prior[k]-b)/float(radiusB));shape+=distance*distance;}
   if(std::sqrt(std::min(sum,reversed)/corners.size())<.04*std::hypot(w,h)&&std::sqrt(shape/corners.size())<.04){s.detection=3;return int(s.views.size());}
  }
  if(s.views.size()>=30)s.views.erase(s.views.begin());
  s.width=w;s.height=h;s.views.push_back(corners);s.detection=4;++s.revision;return int(s.views.size());
 }catch(...){return -1;}
}
extern "C" WSScreenLensProgress WSScreenLensGetProgress(void*p){
 WSScreenLensProgress out={};if(!p)return out;
 try{
  auto&s=*static_cast<ScreenLens*>(p);out.count=int(s.views.size());out.detection=s.detection;out.revision=s.revision;
  if(!s.width||!s.height)return out;
  out.x=double(s.lastBox.x)/s.width;out.y=double(s.lastBox.y)/s.height;out.width=double(s.lastBox.width)/s.width;out.height=double(s.lastBox.height)/s.height;
  if(s.views.empty())return out;
  std::vector<cv::Point2f> all,centres;
  for(auto&v:s.views){cv::Point2f c(0,0);for(auto&q:v){all.push_back(q);c+=q;}centres.push_back(c/float(v.size()));}
  std::vector<cv::Point2f> hull;cv::convexHull(all,hull);auto b=cv::boundingRect(centres);
  out.coverage=cv::contourArea(hull)/(s.width*s.height);
  out.min_x=double(b.x)/s.width;out.max_x=double(b.x+b.width)/s.width;
  out.min_y=double(b.y)/s.height;out.max_y=double(b.y+b.height)/s.height;
  out.coverage_ready=out.coverage>=.4&&b.width>=.3*s.width&&b.height>=.3*s.height;
 }catch(...){}return out;
}
extern "C" WSLocalization WSScreenLensSolve(void*p){
 WSLocalization out={};out.reason=1;if(!p)return out;
 try{
  auto&s=*static_cast<ScreenLens*>(p);out.matches=int(s.views.size());if(s.views.size()<12)return out;
  std::vector<cv::Point2f> all,centres;
  for(auto&v:s.views){cv::Point2f centre(0,0);for(auto&q:v){all.push_back(q);centre+=q;}centres.push_back(centre/float(v.size()));}
  std::vector<cv::Point2f> hull;cv::convexHull(all,hull);auto bounds=cv::boundingRect(centres);
  out.reason=3;if(cv::contourArea(hull)<.4*s.width*s.height||bounds.width<.3*s.width||bounds.height<.3*s.height)return out;
  auto pattern=screenPattern();cv::Mat firstK,firstD;double totalError=0;int tested=0;
  for(int fold=0;fold<4;++fold){
   std::vector<std::vector<cv::Point2f>> images;std::vector<std::vector<cv::Point3f>> objects;
   for(size_t i=0;i<s.views.size();++i)if(int(i)%4!=fold){images.push_back(s.views[i]);objects.push_back(pattern);}
   cv::Mat K=(cv::Mat_<double>(3,3)<<s.width,0,s.width/2.,0,s.width,s.height/2.,0,0,1),D=cv::Mat::zeros(5,1,CV_64F),sdK,sdPose,errors;
   std::vector<cv::Mat> rotations,translations;
   int flags=cv::CALIB_USE_INTRINSIC_GUESS|cv::CALIB_FIX_ASPECT_RATIO|cv::CALIB_ZERO_TANGENT_DIST|cv::CALIB_FIX_K3;
   out.reason=4;
   double rms=cv::calibrateCamera(objects,images,cv::Size(s.width,s.height),K,D,rotations,translations,sdK,sdPose,errors,flags);
   if(rms>1.5||!validLens(K,D,s.width,s.height)||!cv::checkRange(sdK)||sdK.at<double>(1)>K.at<double>(1,1)*.05||sdK.at<double>(2)>s.width*.03||sdK.at<double>(3)>s.height*.03)return out;
   // Require independently tilted views; moving a front-facing screen alone
   // leaves focal length and distance ambiguous.
   double tiltSpread=0;
   for(size_t a=0;a<rotations.size();++a)for(size_t b=0;b<a;++b){cv::Mat A,B;cv::Rodrigues(rotations[a],A);cv::Rodrigues(rotations[b],B);tiltSpread=std::max(tiltSpread,cv::norm(A.col(2)-B.col(2)));}
   if(tiltSpread<.35)return out;
   if(fold==0){firstK=K;firstD=D;}
   else if(std::abs(K.at<double>(0,0)/firstK.at<double>(0,0)-1)>.05||std::abs(K.at<double>(0,2)-firstK.at<double>(0,2))>.03*s.width||std::abs(K.at<double>(1,2)-firstK.at<double>(1,2))>.03*s.height)return out;
   for(size_t i=0;i<s.views.size();++i)if(int(i)%4==fold){
    std::vector<cv::Point3f> train,hold;std::vector<cv::Point2f> trainUV,holdUV;
    for(size_t j=0;j<pattern.size();++j)if(j%4==0){hold.push_back(pattern[j]);holdUV.push_back(s.views[i][j]);}else{train.push_back(pattern[j]);trainUV.push_back(s.views[i][j]);}
    cv::Mat r,t;out.reason=5;
    if(!cv::solvePnP(train,trainUV,K,D,r,t,false,cv::SOLVEPNP_ITERATIVE))return out;
    double error=reprojection(hold,holdUV,K,D,r,t,4);if(error>1.5)return out;
    totalError+=error*error;++tested;
   }
  }
  out.fx=firstK.at<double>(0,0);out.fy=firstK.at<double>(1,1);out.cx=firstK.at<double>(0,2);out.cy=firstK.at<double>(1,2);
  for(int j=0;j<5;++j)out.distortion[j]=firstD.at<double>(j);
  out.rms=std::sqrt(totalError/tested);out.inliers=int(s.views.size())*54;out.fitted_lens=1;out.reason=0;out.success=1;return out;
 }catch(...){out.reason=6;return out;}
}

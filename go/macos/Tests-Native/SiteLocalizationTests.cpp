#include "../Sources/Shared/SiteLocalization.h"
#include <opencv2/core.hpp>
#include <opencv2/calib3d.hpp>
#include <opencv2/imgproc.hpp>
#include <cassert>
#include <chrono>
#include <iostream>
// Render a raw distorted sensor image from an ideal pinhole image. Each output
// pixel is independently mapped through the inverse camera model.
static cv::Mat distort(const cv::Mat& image,const cv::Mat& K,const cv::Mat& D){
 std::vector<cv::Point2f> raw,ideal;
 for(int y=0;y<image.rows;++y)for(int x=0;x<image.cols;++x)raw.emplace_back(x,y);
 cv::undistortPoints(raw,ideal,K,D,cv::noArray(),K);
 cv::Mat mx(image.size(),CV_32F),my(image.size(),CV_32F);
 for(size_t i=0;i<ideal.size();++i){mx.at<float>(int(i)/image.cols,int(i)%image.cols)=ideal[i].x;my.at<float>(int(i)/image.cols,int(i)%image.cols)=ideal[i].y;}
 cv::Mat result;cv::remap(image,result,mx,my,cv::INTER_LINEAR,cv::BORDER_CONSTANT,cv::Scalar(150));return result;
}
int main(){
 cv::setNumThreads(2);cv::RNG random(59271);
 cv::Mat K=(cv::Mat_<double>(3,3)<<570,0,315,0,570,243,0,0,1),D=(cv::Mat_<double>(5,1)<<.015,-.004,0,0,0);
 cv::Mat r=(cv::Mat_<double>(3,1)<<.03,-.08,.01),t=(cv::Mat_<double>(3,1)<<.1,-.08,.2);
 std::vector<cv::Point3f> points;std::vector<cv::Point2f> pixels;
 for(int i=0;i<500;++i){cv::Point3f p(random.uniform(-1.7f,1.7f),random.uniform(-1.3f,1.3f),random.uniform(2.f,5.f));std::vector<cv::Point2f> q;cv::projectPoints(std::vector<cv::Point3f>{p},r,t,K,D,q);if(q[0].x>5&&q[0].x<635&&q[0].y>5&&q[0].y<475){points.push_back(p);pixels.push_back(q[0]);}}
 std::vector<double> xyz,uv;for(size_t i=0;i<points.size();++i){auto p=points[i];auto q=pixels[i];xyz.insert(xyz.end(),{p.x,p.y,p.z});uv.insert(uv.end(),{q.x,q.y});}
 auto fitted=WSSolveCamera(xyz.data(),uv.data(),int(points.size()),640,480,nullptr);
 std::cout<<"Joint fit success="<<fitted.success<<" reason="<<fitted.reason<<" fx="<<fitted.fx<<" rms="<<fitted.rms<<" inliers="<<fitted.inliers<<std::endl;
 assert(fitted.success&&fitted.fitted_lens&&std::abs(fitted.fx-570)<6&&std::abs(fitted.cx-315)<3&&fitted.rms<.3);
 cv::Mat R;cv::Rodrigues(r,R);cv::Mat expected=-R.t()*t;for(int i=0;i<3;++i)assert(std::abs(fitted.world_from_camera[12+i]-expected.at<double>(i))<.015);
 double known[]={570,570,315,243,.015,-.004,0,0,0};
 auto aligned=WSSolveCamera(xyz.data(),uv.data(),int(points.size()),640,480,known);assert(aligned.success&&!aligned.fitted_lens);
 auto corrupted=uv;for(size_t i=0;i<corrupted.size()/2;i+=5){corrupted[i*2]=random.uniform(1.,639.);corrupted[i*2+1]=random.uniform(1.,479.);}
 auto robust=WSSolveCamera(xyz.data(),corrupted.data(),int(points.size()),640,480,nullptr);assert(robust.success&&std::abs(robust.fx-570)<8);
 auto noisyXYZ=xyz,noisyUV=uv;
 for(auto&v:noisyXYZ)v+=random.gaussian(.005);
 for(auto&v:noisyUV)v+=random.gaussian(.15);
 auto noisy=WSSolveCamera(noisyXYZ.data(),noisyUV.data(),int(points.size()),640,480,nullptr);
 assert(noisy.success&&std::abs(noisy.fx-570)<15&&noisy.rms<2);
 // Preserve the same geometric evidence when only capture resolution and
 // pixel units change, including the move from 640x480 to 1600x1200.
 auto resolutionUV=uv;
 for(size_t i=0;i<points.size();++i){
  resolutionUV[i*2]+=1.6*std::sin(i*.73);
  resolutionUV[i*2+1]+=1.6*std::cos(i*.91);
 }
 cv::setRNGSeed(431);
 auto reference=WSSolveCamera(xyz.data(),resolutionUV.data(),int(points.size()),640,480,known);
 assert(reference.success);
 for(double scale:{.5,1.25,2.5}){
  auto scaledUV=resolutionUV;for(auto&pixel:scaledUV)pixel*=scale;
  double scaledLens[9];std::copy(known,known+9,scaledLens);
  for(int i=0;i<4;++i)scaledLens[i]*=scale;
  cv::setRNGSeed(431);
  auto scaled=WSSolveCamera(xyz.data(),scaledUV.data(),int(points.size()),int(640*scale),int(480*scale),scaledLens);
  std::cout<<"Resolution "<<scale<<": success="<<scaled.success<<" inliers="<<scaled.inliers<<" rms="<<scaled.rms<<std::endl;
  assert(scaled.success&&scaled.inliers>=reference.inliers*.95);
  assert(std::abs(scaled.rms/scale-reference.rms)<.15);
  for(int axis=0;axis<3;++axis)assert(std::abs(scaled.world_from_camera[12+axis]-reference.world_from_camera[12+axis])<.02);
 }
 // Sparse, well-distributed correspondences should be judged by their
 // geometry and withheld-point accuracy, not an arbitrary sixty-point floor.
 for(int count:{17,21}){
  auto sparse=WSSolveCamera(xyz.data(),uv.data(),count,640,480,nullptr);
  std::cout<<"Sparse "<<count<<": success="<<sparse.success<<" reason="<<sparse.reason<<" inliers="<<sparse.inliers<<" rms="<<sparse.rms<<std::endl;
  assert(sparse.success&&std::abs(sparse.fx-570)<8&&sparse.rms<.3);
 }
 auto sparseKnown=WSSolveCamera(xyz.data(),uv.data(),12,640,480,known);
 assert(sparseKnown.success&&!sparseKnown.fitted_lens);
 assert(!WSSolveCamera(xyz.data(),uv.data(),15,640,480,nullptr).success);
 assert(!WSSolveCamera(xyz.data(),uv.data(),11,640,480,known).success);
 auto badSparse=uv;
 for(int i=0;i<21;++i){badSparse[i*2]=random.uniform(1.,639.);badSparse[i*2+1]=random.uniform(1.,479.);}
 assert(!WSSolveCamera(xyz.data(),badSparse.data(),21,640,480,nullptr).success);
 auto tight=uv;
 for(int i=0;i<21;++i){tight[i*2]=320+(uv[i*2]-320)*.15;tight[i*2+1]=240+(uv[i*2+1]-240)*.15;}
 assert(!WSSolveCamera(xyz.data(),tight.data(),21,640,480,nullptr).success);
 std::vector<cv::Point3f> planar=points;for(auto&p:planar)p.z=3;cv::projectPoints(planar,r,t,K,D,pixels);
 xyz.clear();uv.clear();
 for(size_t i=0;i<planar.size();++i){if(pixels[i].x<0||pixels[i].x>=640||pixels[i].y<0||pixels[i].y>=480)continue;xyz.insert(xyz.end(),{planar[i].x,planar[i].y,3});uv.insert(uv.end(),{pixels[i].x,pixels[i].y});}
 auto flat=WSSolveCamera(xyz.data(),uv.data(),int(uv.size()/2),640,480,nullptr);
 assert(!flat.success&&flat.reason==7);
 auto knownFlat=WSSolveCamera(xyz.data(),uv.data(),int(uv.size()/2),640,480,known);
 assert(knownFlat.success&&!knownFlat.fitted_lens);
 auto estimatedFlat=WSSolveCameraEstimated(xyz.data(),uv.data(),int(uv.size()/2),640,480);
 // A wall can fit several focal lengths at very different distances. Low
 // reprojection error alone must not turn that ambiguity into a saved pose.
 assert(!estimatedFlat.success&&estimatedFlat.reason==8);


 // An assumed lens can support 8–10 independent points, but never upgrades
 // them to a measured calibration or accepts an unstable / concentrated fit.
 std::vector<double> smallXYZ,smallUV;
 for(int i=0;i<10;++i){
  double x=(i%3-1)*.8,y=(i/3-1.5)*.55,z=3+.2*(i%4);
  smallXYZ.insert(smallXYZ.end(),{x,y,z});
  smallUV.insert(smallUV.end(),{640*x/z+320,640*y/z+240});
 }
 for(int count:{8,9,10}){
  auto few=WSSolveCameraEstimated(smallXYZ.data(),smallUV.data(),count,640,480);
  std::cout<<"Estimated "<<count<<": success="<<few.success<<" reason="<<few.reason<<" rms="<<few.rms<<std::endl;
  assert(few.success&&few.fitted_lens==2&&few.inliers>=8&&few.rms<.01);
  for(int axis=0;axis<3;++axis)assert(std::abs(few.world_from_camera[12+axis])<.01);
  auto measuredFew=WSSolveCamera(smallXYZ.data(),smallUV.data(),count,640,480,nullptr);
  assert(!measuredFew.success);
 }
 assert(!WSSolveCameraEstimated(smallXYZ.data(),smallUV.data(),7,640,480).success);
 // Recover a focal length between the old discrete guesses. The same image
 // evidence must constrain depth, not merely produce a plausible direction.
 auto offGridUV=smallUV;
 for(int i=0;i<10;++i){
  offGridUV[i*2]=530*smallXYZ[i*3]/smallXYZ[i*3+2]+320;
  offGridUV[i*2+1]=530*smallXYZ[i*3+1]/smallXYZ[i*3+2]+240;
 }
 auto offGrid=WSSolveCameraEstimated(smallXYZ.data(),offGridUV.data(),10,640,480);
 std::cout<<"Off-grid focal: success="<<offGrid.success<<" reason="<<offGrid.reason<<" fx="<<offGrid.fx<<" z="<<offGrid.world_from_camera[14]<<std::endl;
 assert(offGrid.success&&offGrid.fitted_lens==2&&std::abs(offGrid.fx-530)<1);
 for(int axis=0;axis<3;++axis)assert(std::abs(offGrid.world_from_camera[12+axis])<.01);
 auto clustered=smallUV;
 for(int i=0;i<10;++i){clustered[i*2]=320+(clustered[i*2]-320)*.1;clustered[i*2+1]=240+(clustered[i*2+1]-240)*.1;}
 assert(!WSSolveCameraEstimated(smallXYZ.data(),clustered.data(),10,640,480).success);
 auto wrong=smallUV;
 for(int i=0;i<10;++i){wrong[i*2]=random.uniform(1.,639.);wrong[i*2+1]=random.uniform(1.,479.);}
 assert(!WSSolveCameraEstimated(smallXYZ.data(),wrong.data(),10,640,480).success);


 auto map=WSMapCreate();assert(map);std::vector<float> data(120*WS_LANDMARK_FLOATS,0);
 assert(WSMapImport(map,data.data(),120)==120);data[4]=std::numeric_limits<float>::quiet_NaN();assert(WSMapImport(map,data.data(),120)==-1);assert(WSMapCount(map)==120);
 WSMapDestroy(map);
 // Exercise feature extraction, depth back-projection, descriptor matching,
 // and optical-to-world conventions together, with a synthetic textured scene.
 cv::Mat scene(480,640,CV_8U,cv::Scalar(110));
 for(int i=0;i<1800;++i)cv::circle(scene,cv::Point(random.uniform(0,640),random.uniform(0,480)),random.uniform(2,9),cv::Scalar(random.uniform(0,255)),-1);
 std::vector<float> depth(320*240);std::vector<uint8_t> confidence(depth.size(),2);
 for(int y=0;y<240;++y)for(int x=0;x<320;++x)depth[y*320+x]=3+.6*std::sin(x/70.)+.5*std::cos(y/65.);
 double captureK[]={570,570,315,243},captureT[]={1,0,0,0,0,-1,0,0,0,0,-1,0,1,1.5,2,1};
 map=WSMapCreate();int landmarks=WSMapAdd(map,scene.data,640,480,depth.data(),confidence.data(),320,240,captureK,captureT);
 assert(landmarks>=120);
 double sceneLens[]={570,570,315,243,0,0,0,0,0};
 auto pipeline=WSLocalize(map,scene.data,640,480,sceneLens);
 std::cout<<"Map pipeline: landmarks="<<landmarks<<" success="<<pipeline.success<<" reason="<<pipeline.reason<<" rms="<<pipeline.rms<<std::endl;
 assert(pipeline.success);
 for(int i=0;i<16;++i)assert(std::abs(pipeline.world_from_camera[i]-captureT[i])<.01);
 // Strong barrel distortion must be corrected before descriptor extraction,
 // with the same measured lens retained for raw-pixel pose validation.
 cv::Mat barrelD=(cv::Mat_<double>(5,1)<<-.35,.08,0,0,0);
 auto rawScene=distort(scene,K,barrelD);
 double barrelLens[]={570,570,315,243,-.35,.08,0,0,0};
 auto corrected=WSLocalize(map,rawScene.data,640,480,barrelLens);
 std::cout<<"Distorted pipeline: success="<<corrected.success<<" reason="<<corrected.reason<<" matches="<<corrected.matches<<" rms="<<corrected.rms<<std::endl;
 assert(corrected.success&&!corrected.fitted_lens);
 for(int i=0;i<16;++i)assert(std::abs(corrected.world_from_camera[i]-captureT[i])<.025);
 for(int i=0;i<5;++i)assert(std::abs(corrected.distortion[i]-barrelLens[4+i])<1e-9);
 // Multiple scan observations of the same feature must not reject its match
 // as ambiguous, or count as independent geometric support.
 std::vector<float> exported(size_t(landmarks)*WS_LANDMARK_FLOATS);
 assert(WSMapExport(map,exported.data(),landmarks)==landmarks);
 auto repeated=exported;
 for(int i=0;i<landmarks;++i){auto begin=exported.begin()+i*WS_LANDMARK_FLOATS;repeated.insert(repeated.end(),begin,begin+WS_LANDMARK_FLOATS);repeated[repeated.size()-WS_LANDMARK_FLOATS]+=.01f;}
 assert(WSMapImport(map,repeated.data(),landmarks*2)==landmarks*2);
 auto duplicates=WSLocalize(map,scene.data,640,480,sceneLens);
 assert(duplicates.success&&duplicates.matches<=pipeline.matches+3);
 cv::Mat changed;scene.convertTo(changed,CV_8U,.7,35);
 auto relit=WSLocalize(map,changed.data,640,480,sceneLens);
 assert(relit.success);
 for(int i=0;i<16;++i)assert(std::abs(relit.world_from_camera[i]-captureT[i])<.02);
 cv::Mat blank(480,640,CV_8U,cv::Scalar(110));
 assert(!WSLocalize(map,blank.data,640,480,nullptr).success);

 WSMapDestroy(map);
 // A full map must still learn a later viewpoint, while retaining earlier
 // room geometry and respecting the storage bound. Previously add returned
 // immediately at capacity, silently discarding the rest of the room scan.
 map=WSMapCreate();
 std::vector<float> fullMap;
 for(int i=0;i<WS_MAX_LANDMARKS;++i){
  int original=i%landmarks;
  auto start=exported.begin()+original*WS_LANDMARK_FLOATS;
  fullMap.insert(fullMap.end(),start,start+WS_LANDMARK_FLOATS);
  fullMap[fullMap.size()-WS_LANDMARK_FLOATS]+=20+5*(i/landmarks);
 }
 assert(WSMapImport(map,fullMap.data(),WS_MAX_LANDMARKS)==WS_MAX_LANDMARKS);
 cv::Mat later(480,640,CV_8U,cv::Scalar(110));
 for(int i=0;i<1800;++i)cv::circle(later,cv::Point(random.uniform(0,640),random.uniform(0,480)),random.uniform(2,9),cv::Scalar(random.uniform(0,255)),-1);
 assert(WSMapAdd(map,later.data,640,480,depth.data(),confidence.data(),320,240,captureK,captureT)==WS_MAX_LANDMARKS);
 std::vector<float> retained(size_t(WS_MAX_LANDMARKS)*WS_LANDMARK_FLOATS);
 assert(WSMapExport(map,retained.data(),WS_MAX_LANDMARKS)==WS_MAX_LANDMARKS);
 int laterPoints=0;
 for(int i=0;i<WS_MAX_LANDMARKS;++i)if(retained[i*WS_LANDMARK_FLOATS]<10)++laterPoints;
 assert(laterPoints>=120&&laterPoints<2000);
 auto laterPose=WSLocalize(map,later.data,640,480,sceneLens);
 std::cout<<"Full map later viewpoint: retained="<<laterPoints<<" success="<<laterPose.success<<" reason="<<laterPose.reason<<std::endl;
 assert(laterPose.success);
 for(int i=0;i<16;++i)assert(std::abs(laterPose.world_from_camera[i]-captureT[i])<.02);
 WSMapDestroy(map);
 // Render a phone-screen target at independently varied poses. This tests
 // detection and calibration together, including rejection of repeated views.
 auto screen=WSScreenLensCreate();assert(screen);
 auto curvedScreen=WSScreenLensCreate();assert(curvedScreen);int curvedAccepted=0;
 auto wideScreen=WSScreenLensCreate();assert(wideScreen);
 auto flatScreen=WSScreenLensCreate();assert(flatScreen);
 cv::Mat board(480,360,CV_8U,cv::Scalar(255));
 for(int y=0;y<10;++y)for(int x=0;x<7;++x)if((x+y)%2==0)cv::rectangle(board,cv::Rect((x+1)*40,(y+1)*40,40,40),cv::Scalar(0),-1);
 cv::Mat targetK=(cv::Mat_<double>(3,3)<<570,0,320,0,570,240,0,0,1);
 std::vector<cv::Point3f> outer{{-2,-2,0},{7,-2,0},{7,10,0},{-2,10,0}};
 std::vector<cv::Point2f> source{{0,0},{360,0},{360,480},{0,480}};
 int accepted=0;
 for(int i=0;i<30;++i){
  double ax=(i%3-1)*.35+(i/9)*.12,ay=((i/3)%3-1)*.35-(i/9)*.10,z=23+(i%2)*3;
  cv::Mat rotation=(cv::Mat_<double>(3,1)<<ax,ay,.05*(i%2)),matrix;cv::Rodrigues(rotation,matrix);
  double u=180+(i%3)*140,v=150+((i/3)%3)*90;
  cv::Mat centre=(cv::Mat_<double>(3,1)<<(u-320)*z/570,(v-240)*z/570,z),offset=(cv::Mat_<double>(3,1)<<2.5,4,0);
  cv::Mat translation=centre-matrix*offset;
  std::vector<cv::Point2f> destination;cv::projectPoints(outer,rotation,translation,targetK,cv::Mat(),destination);
  cv::Mat transform=cv::getPerspectiveTransform(source,destination),view;
  cv::warpPerspective(board,view,transform,cv::Size(640,480),cv::INTER_LINEAR,cv::BORDER_CONSTANT,cv::Scalar(150));
  cv::GaussianBlur(view,view,cv::Size(3,3),.4);
  auto rawView=distort(view,targetK,barrelD);
  curvedAccepted=WSScreenLensAdd(curvedScreen,rawView.data,640,480);
  // Independently render the Go2's 1280x720 mode, covering its wider frame.
  cv::Mat wideK=(cv::Mat_<double>(3,3)<<855,0,640,0,855,360,0,0,1);
  double wideU=240+(i%3)*400,wideV=200+((i/3)%3)*160;
  cv::Mat wideCentre=(cv::Mat_<double>(3,1)<<(wideU-640)*z/855,(wideV-360)*z/855,z);
  cv::Mat wideTranslation=wideCentre-matrix*offset;
  cv::projectPoints(outer,rotation,wideTranslation,wideK,cv::Mat(),destination);
  cv::Mat wideIdeal;
  cv::warpPerspective(board,wideIdeal,cv::getPerspectiveTransform(source,destination),cv::Size(1280,720),cv::INTER_LINEAR,cv::BORDER_CONSTANT,cv::Scalar(150));
  auto wide=distort(wideIdeal,wideK,barrelD);
  assert(WSScreenLensAdd(wideScreen,wide.data,1280,720)>=0);
  // Image coverage alone must not make front-facing views a calibration.
  double frontU=120+(i%5)*100,frontV=130+((i/5)%3)*110,frontZ=27;
  cv::Mat frontTranslation=(cv::Mat_<double>(3,1)<<(frontU-320)*frontZ/570-2.5,(frontV-240)*frontZ/570-4,frontZ);
  cv::projectPoints(outer,cv::Vec3d(),frontTranslation,targetK,cv::Mat(),destination);
  cv::Mat front;
  cv::warpPerspective(board,front,cv::getPerspectiveTransform(source,destination),cv::Size(640,480),cv::INTER_LINEAR,cv::BORDER_CONSTANT,cv::Scalar(150));
  assert(WSScreenLensAdd(flatScreen,front.data,640,480)>=0);
  accepted=WSScreenLensAdd(screen,view.data,640,480);assert(accepted>=0);
  const auto capturedProgress=WSScreenLensGetProgress(screen);
  assert(capturedProgress.count==accepted);
  assert(WSScreenLensAdd(screen,view.data,640,480)==accepted);
  const auto repeatedProgress=WSScreenLensGetProgress(screen);
  assert(repeatedProgress.revision==capturedProgress.revision);
  if(accepted>0)assert(repeatedProgress.detection==3||repeatedProgress.detection==0||repeatedProgress.detection==1||repeatedProgress.detection==2);
 }
 auto coverageProgress=WSScreenLensGetProgress(screen);
 assert(coverageProgress.count>=12&&coverageProgress.coverage_ready&&coverageProgress.coverage>=.4);
 auto solveStarted=std::chrono::steady_clock::now();
 auto measured=WSScreenLensSolve(screen);
 std::cout<<"Screen solve seconds="<<std::chrono::duration<double>(std::chrono::steady_clock::now()-solveStarted).count()<<std::endl;
 std::cout<<"Screen calibration: views="<<accepted<<" success="<<measured.success<<" reason="<<measured.reason<<" fx="<<measured.fx<<" rms="<<measured.rms<<std::endl;
 assert(accepted>=12&&measured.success&&std::abs(measured.fx-570)<20&&measured.rms<1);
 auto curvedMeasured=WSScreenLensSolve(curvedScreen);
 std::cout<<"Barrel calibration: views="<<curvedAccepted<<" success="<<curvedMeasured.success<<" reason="<<curvedMeasured.reason<<" fx="<<curvedMeasured.fx<<" rms="<<curvedMeasured.rms<<std::endl;
 assert(curvedAccepted>=12&&curvedMeasured.success&&std::abs(curvedMeasured.fx-570)<20&&curvedMeasured.rms<1);
 const auto wideMeasured=WSScreenLensSolve(wideScreen);
 std::cout<<"1280x720 calibration: success="<<wideMeasured.success<<" reason="<<wideMeasured.reason<<" fx="<<wideMeasured.fx<<" rms="<<wideMeasured.rms<<std::endl;
 assert(wideMeasured.success&&std::abs(wideMeasured.fx-855)<30&&std::abs(wideMeasured.cx-640)<12&&wideMeasured.rms<1.5);
 const auto flatProgress=WSScreenLensGetProgress(flatScreen);
 assert(flatProgress.count>=12&&flatProgress.coverage_ready);
 const auto flatMeasured=WSScreenLensSolve(flatScreen);
 std::cout<<"Front-facing calibration rejected: reason="<<flatMeasured.reason<<std::endl;
 assert(!flatMeasured.success);
 WSScreenLensDestroy(wideScreen);WSScreenLensDestroy(flatScreen);
 // Check independent rays across the frame; individual polynomial coefficients
 // can compensate for each other despite small image error.
 cv::Mat measuredK=(cv::Mat_<double>(3,3)<<curvedMeasured.fx,0,curvedMeasured.cx,0,curvedMeasured.fy,curvedMeasured.cy,0,0,1);
 cv::Mat measuredD(5,1,CV_64F,curvedMeasured.distortion);
 std::vector<cv::Point3f> rays;
 for(float y:{-.38f,0.f,.38f})for(float x:{-.52f,0.f,.52f})rays.emplace_back(x,y,1);
 std::vector<cv::Point2f> actualPixels,measuredPixels;
 cv::projectPoints(rays,cv::Vec3d(),cv::Vec3d(),targetK,barrelD,actualPixels);
 cv::projectPoints(rays,cv::Vec3d(),cv::Vec3d(),measuredK,measuredD,measuredPixels);
 for(size_t i=0;i<rays.size();++i)assert(cv::norm(actualPixels[i]-measuredPixels[i])<4);
 const auto beforeBlank=WSScreenLensGetProgress(curvedScreen);
 assert(WSScreenLensAdd(curvedScreen,blank.data,640,480)==beforeBlank.count);
 const auto afterBlank=WSScreenLensGetProgress(curvedScreen);
 assert(afterBlank.detection==0&&afterBlank.revision==beforeBlank.revision&&afterBlank.coverage_ready);
 WSScreenLensDestroy(curvedScreen);
 WSScreenLensDestroy(screen);
 screen=WSScreenLensCreate();assert(!WSScreenLensSolve(screen).success);WSScreenLensDestroy(screen);
 std::cout<<"Localization: joint lens/pose, fixed lens, outliers, planar refusal, map validation passed\n";
}

#include "../Sources/Shared/SiteLocalization.h"
#include <opencv2/core.hpp>
#include <opencv2/calib3d.hpp>
#include <opencv2/imgproc.hpp>
#include <cassert>
#include <iostream>
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
 assert(estimatedFlat.success&&estimatedFlat.fitted_lens==2);


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
 // Render a phone-screen target at independently varied poses. This tests
 // detection and calibration together, including rejection of repeated views.
 auto screen=WSScreenLensCreate();assert(screen);
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
  accepted=WSScreenLensAdd(screen,view.data,640,480);assert(accepted>=0);
  assert(WSScreenLensAdd(screen,view.data,640,480)==accepted);
 }
 auto measured=WSScreenLensSolve(screen);
 std::cout<<"Screen calibration: views="<<accepted<<" success="<<measured.success<<" reason="<<measured.reason<<" fx="<<measured.fx<<" rms="<<measured.rms<<std::endl;
 assert(accepted>=12&&measured.success&&std::abs(measured.fx-570)<20&&measured.rms<1);
 WSScreenLensDestroy(screen);
 screen=WSScreenLensCreate();assert(!WSScreenLensSolve(screen).success);WSScreenLensDestroy(screen);
 std::cout<<"Localization: joint lens/pose, fixed lens, outliers, planar refusal, map validation passed\n";
}

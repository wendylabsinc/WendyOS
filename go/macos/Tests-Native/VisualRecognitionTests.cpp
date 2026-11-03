#include "../Sources/Shared/VisualRecognition.h"
#include <opencv2/core.hpp>
#include <opencv2/imgproc.hpp>
#include <opencv2/calib3d.hpp>
#include <cassert>
#include <iostream>

int main() {
    cv::setNumThreads(2);
    cv::Mat reference(600,800,CV_8U,cv::Scalar(120));
    cv::RNG rng(16384);
    for (int i=0;i<600;++i) {
        cv::Point p(rng.uniform(110,690),rng.uniform(110,490));
        cv::circle(reference,p,rng.uniform(2,10),cv::Scalar(rng.uniform(0,255)),-1);
    }
    auto handle=WVRecognizerCreate(); assert(handle);
    double quad[]={100,100,700,100,700,500,100,500};
    assert(WVRecognizerAdd(handle,reference.data,800,600,quad,0.3,0.2)==0);
    assert(WVRecognizerAdd(handle,reference.data,800,600,quad,-1,0)==-1);
    cv::Mat K=(cv::Mat_<double>(3,3)<<800,0,400,0,800,300,0,0,1);
    cv::Mat rotation=(cv::Mat_<double>(3,1)<<0.2,-0.35,0.12), translation=(cv::Mat_<double>(3,1)<<0.02,-0.01,0.65);
    std::vector<cv::Point3f> object={{-.15f,-.1f,0},{.15f,-.1f,0},{.15f,.1f,0},{-.15f,.1f,0}};
    std::vector<cv::Point2f> target;cv::projectPoints(object,rotation,translation,K,cv::noArray(),target);
    std::vector<cv::Point2f> source={{100,100},{700,100},{700,500},{100,500}};
    cv::Mat frame;cv::warpPerspective(reference,frame,cv::getPerspectiveTransform(source,target),reference.size(),cv::INTER_LINEAR,cv::BORDER_CONSTANT,cv::Scalar(120));
    auto detected=WVRecognizerDetect(handle,frame.data,800,600,800,800,400,300);
    assert(!detected.failed && detected.count==1);
    auto &match=detected.items[0];assert(match.inliers>=16);
    for(int i=0;i<4;++i){assert(std::abs(match.corners[i*2]*800-target[i].x)<3);assert(std::abs(match.corners[i*2+1]*600-target[i].y)<3);}
    assert(match.pose_valid);
    for(int i=0;i<3;++i)assert(std::abs(match.object_to_camera[12+i]-translation.at<double>(i))<.01);
    cv::Mat expected;cv::Rodrigues(rotation,expected);
    for(int row=0;row<3;++row)for(int col=0;col<3;++col)assert(std::abs(match.object_to_camera[col*4+row]-expected.at<double>(row,col))<.04);
    // Pixels without a calibrated physical reference must never create a pose.
    auto appearance=WVRecognizerCreate();
    assert(WVRecognizerAdd(appearance,reference.data,800,600,quad,0,0)==0);
    auto uncalibrated=WVRecognizerDetect(appearance,frame.data,800,600,800,800,400,300);
    assert(uncalibrated.count==1 && !uncalibrated.items[0].pose_valid);
    // No detections may leak from a preceding frame into a blank/negative image.
    cv::Mat blank(600,800,CV_8U,cv::Scalar(127));
    assert(WVRecognizerDetect(handle,blank.data,800,600,800,800,400,300).count==0);
    cv::Mat noise(600,800,CV_8U);rng.fill(noise,cv::RNG::UNIFORM,0,255);
    assert(WVRecognizerDetect(handle,noise.data,800,600,800,800,400,300).count==0);
    cv::Mat mirrored;cv::flip(frame,mirrored,1);
    assert(WVRecognizerDetect(handle,mirrored.data,800,600,800,800,400,300).count==0);
    assert(WVRecognizerDetect(handle,nullptr,800,600,0,0,0,0).failed);
    WVRecognizerDestroy(handle);WVRecognizerDestroy(appearance);
    std::cout << "Native recognition: perspective, metric pose, uncalibrated appearance, blank, noise, mirror, and invalid input passed\n";
}

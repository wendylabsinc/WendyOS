#include "../Shared/VisualRecognition.h"
#include <opencv2/core.hpp>
#include <opencv2/imgproc.hpp>
#include <opencv2/features2d.hpp>
#include <opencv2/calib3d.hpp>
#include <cmath>
#include <vector>
#include <algorithm>

namespace {
struct Reference {
    std::vector<cv::KeyPoint> points;
    cv::Mat descriptors;
    std::vector<cv::Point2f> quad;
    cv::Mat imageToPlane;
    double width, height;
};
struct Recognizer {
    cv::Ptr<cv::SIFT> sift = cv::SIFT::create(1800);
    std::vector<Reference> references;
};
bool validSize(int w, int h) { return w >= 16 && h >= 16 && w <= 4096 && h <= 4096 && int64_t(w)*h <= 12582912; }
bool isFinite(double v) { return std::isfinite(v); }
}

extern "C" void *WVRecognizerCreate() {
    try { return new Recognizer(); } catch (...) { return nullptr; }
}
extern "C" void WVRecognizerDestroy(void *p) { delete static_cast<Recognizer *>(p); }

extern "C" int32_t WVRecognizerAdd(void *ptr, const uint8_t *gray, int32_t w, int32_t h,
                                    const double *quad, double pw, double ph) {
    if (!ptr || !gray || !quad || !validSize(w,h) || !isFinite(pw) || !isFinite(ph) || pw<0 || ph<0 || pw>10 || ph>10) return -1;
    auto &recognizer = *static_cast<Recognizer *>(ptr);
    if (recognizer.references.size() >= 32) return -1;
    try {
        Reference ref; ref.width=pw; ref.height=ph;
        std::vector<cv::Point> polygon;
        for (int i=0; i<4; ++i) {
            if (!isFinite(quad[i*2]) || !isFinite(quad[i*2+1]) || quad[i*2]<0 || quad[i*2]>=w || quad[i*2+1]<0 || quad[i*2+1]>=h) return -1;
            ref.quad.emplace_back(quad[i*2],quad[i*2+1]);
            polygon.emplace_back(cvRound(quad[i*2]),cvRound(quad[i*2+1]));
        }
        if (!cv::isContourConvex(polygon) || std::abs(cv::contourArea(polygon))<256) return -1;
        cv::Mat mask=cv::Mat::zeros(h,w,CV_8U); cv::fillConvexPoly(mask,polygon,cv::Scalar(255));
        recognizer.sift->detectAndCompute(cv::Mat(h,w,CV_8U,const_cast<uint8_t *>(gray)),mask,ref.points,ref.descriptors);
        if (ref.points.size()<16) return -1;
        if (pw>0 && ph>0) {
            std::vector<cv::Point2f> plane={{float(-pw/2),float(-ph/2)},{float(pw/2),float(-ph/2)},
                                         {float(pw/2),float(ph/2)},{float(-pw/2),float(ph/2)}};
            ref.imageToPlane=cv::getPerspectiveTransform(ref.quad,plane);
        }
        int index=int(recognizer.references.size()); recognizer.references.push_back(std::move(ref)); return index;
    } catch (...) { return -1; }
}

extern "C" WVDetections WVRecognizerDetect(void *ptr, const uint8_t *gray, int32_t w, int32_t h,
                                            double fx, double fy, double cx, double cy) {
    WVDetections output={};
    if (!ptr || !gray || !validSize(w,h)) { output.failed=1; return output; }
    try {
        auto &recognizer=*static_cast<Recognizer *>(ptr);
        output.reference_count = int(recognizer.references.size());
        output.stage = 1;
        cv::Mat full(h,w,CV_8U,const_cast<uint8_t *>(gray)), frame;
        double scale=std::min(1.0,960.0/std::max(w,h));
        cv::resize(full,frame,cv::Size(),scale,scale,cv::INTER_AREA);
        std::vector<cv::KeyPoint> points; cv::Mat descriptors;
        recognizer.sift->detectAndCompute(frame,cv::noArray(),points,descriptors);
        output.frame_keypoints = int(points.size());
        if (points.size()<16) return output;
        output.stage = 2;
        cv::BFMatcher matcher(cv::NORM_L2);
        for (size_t ri=0; ri<recognizer.references.size() && output.count<8; ++ri) {
            auto &ref=recognizer.references[ri];
            std::vector<std::vector<cv::DMatch>> nearest;
            matcher.knnMatch(ref.descriptors,descriptors,nearest,2);
            std::vector<cv::Point2f> source,target;
            std::vector<bool> used(points.size());
            for (auto &pair: nearest) {
                if (pair.size()!=2 || pair[0].distance>=0.70f*pair[1].distance || used[pair[0].trainIdx]) continue;
                used[pair[0].trainIdx]=true;
                source.push_back(ref.points[pair[0].queryIdx].pt);
                target.push_back(points[pair[0].trainIdx].pt);
            }
            output.best_matches = std::max(output.best_matches, int(source.size()));
            if (source.size()<16) continue;
            output.stage = std::max(output.stage, 3);
            cv::Mat inlierMask;
            cv::Mat homography=cv::findHomography(source,target,cv::RANSAC,2.5,inlierMask,1500,0.995);
            if (homography.empty()) continue;
            std::vector<cv::Point2f> a,b,projected;
            for (size_t i=0; i<source.size(); ++i) if (inlierMask.at<uint8_t>(int(i))) { a.push_back(source[i]); b.push_back(target[i]); }
            output.best_inliers = std::max(output.best_inliers, int(a.size()));
            output.stage = std::max(output.stage, 4);
            if (a.size()<16 || a.size()<source.size()*0.50) continue;
            output.stage = std::max(output.stage, 5);
            std::vector<cv::Point2f> hull; cv::convexHull(a,hull);
            if (std::abs(cv::contourArea(hull)) < std::abs(cv::contourArea(ref.quad))*0.12) continue;
            output.stage = std::max(output.stage, 6);
            cv::perspectiveTransform(ref.quad,projected,homography);
            if (!cv::isContourConvex(projected) || cv::contourArea(projected, true)*cv::contourArea(ref.quad, true)<=0) continue;
            output.stage = std::max(output.stage, 7);
            double area=std::abs(cv::contourArea(projected));
            if (area<1600 || area>frame.total()*0.85) continue;
            bool inside=true;
            for (auto &p:projected) if (!isFinite(p.x)||!isFinite(p.y)||p.x<0||p.y<0||p.x>=frame.cols||p.y>=frame.rows) inside=false;
            if (!inside) continue;
            output.stage = std::max(output.stage, 8);
            std::vector<cv::Point2f> predicted; cv::perspectiveTransform(a,predicted,homography);
            double error=0; for (size_t i=0;i<b.size();++i) error+=cv::norm(predicted[i]-b[i])*cv::norm(predicted[i]-b[i]);
            error=std::sqrt(error/b.size()); if (error>2) continue;
            output.stage = 9;
            auto &found=output.items[output.count++];
            found.reference_index=int(ri); found.inliers=int(a.size()); found.rms_pixels=error/scale;
            found.score=double(a.size())/source.size();
            for(int i=0;i<4;++i){found.corners[2*i]=projected[i].x/frame.cols;found.corners[2*i+1]=projected[i].y/frame.rows;}
            if (ref.imageToPlane.empty() || !isFinite(fx)||!isFinite(fy)||!isFinite(cx)||!isFinite(cy)||fx<=0||fy<=0) continue;
            std::vector<cv::Point2f> plane; cv::perspectiveTransform(a,plane,ref.imageToPlane);
            std::vector<cv::Point3f> object; for(auto &p:plane)object.emplace_back(p.x,p.y,0);
            cv::Mat K=(cv::Mat_<double>(3,3)<<fx*scale,0,cx*scale,0,fy*scale,cy*scale,0,0,1);
            std::vector<cv::Mat> rotations,translations; cv::Mat errors;
            cv::solvePnPGeneric(object,b,K,cv::noArray(),rotations,translations,false,cv::SOLVEPNP_IPPE,cv::noArray(),cv::noArray(),errors);
            cv::Mat errors64; errors.convertTo(errors64, CV_64F);
            if(rotations.empty() || errors64.at<double>(0)>2.0 || translations[0].at<double>(2)<=0.05 || translations[0].at<double>(2)>5)continue;
            // Reject the near-equal planar pose alternatives, common head-on.
            if(rotations.size()>1 && errors64.at<double>(1)<errors64.at<double>(0)+0.3)continue;
            cv::Mat R;cv::Rodrigues(rotations[0],R);
            for(int col=0;col<3;++col)for(int row=0;row<3;++row)found.object_to_camera[col*4+row]=R.at<double>(row,col);
            for(int row=0;row<3;++row)found.object_to_camera[12+row]=translations[0].at<double>(row);
            found.object_to_camera[15]=1;found.pose_valid=1;
        }
    } catch (...) { output.count=0;output.failed=1; }
    return output;
}

// Generated texture and depth for adapter verification, never physical evidence.
#include "SiteLocalization.h"
#include <opencv2/core.hpp>
#include <opencv2/imgproc.hpp>
#include <cmath>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <memory>
#include <vector>

int main(int argc, char** argv) {
    if (argc != 2) return 2;
    const std::filesystem::path out(argv[1]);
    if (std::filesystem::exists(out)) return 2;
    std::filesystem::create_directories(out);
    cv::setNumThreads(2);
    cv::RNG random(52791);
    cv::Mat image(480, 640, CV_8U, cv::Scalar(105));
    for (int i = 0; i < 2000; ++i)
        cv::circle(image, {random.uniform(0, 640), random.uniform(0, 480)}, random.uniform(2, 8), cv::Scalar(random.uniform(0, 255)), -1);
    std::vector<float> depth(320 * 240);
    std::vector<uint8_t> confidence(depth.size(), 2);
    for (int y = 0; y < 240; ++y) for (int x = 0; x < 320; ++x)
        depth[y*320+x] = float(3 + .5*std::sin(x/65.) + .4*std::cos(y/75.));
    const double k[] = {570, 570, 320, 240};
    const double transform[] = {1,0,0,0, 0,-1,0,0, 0,0,-1,0, 1,1.5,2,1};
    std::unique_ptr<void, decltype(&WSMapDestroy)> map(WSMapCreate(), WSMapDestroy);
    int count = WSMapAdd(map.get(), image.data, image.cols, image.rows, depth.data(), confidence.data(), 320, 240, k, transform);
    if (count < 120) return 1;
    std::vector<float> points(size_t(count) * WS_LANDMARK_FLOATS);
    if (WSMapExport(map.get(), points.data(), count) != count) return 1;
    std::ofstream landmarks(out / "map.f32", std::ios::binary);
    landmarks.write(reinterpret_cast<char*>(points.data()), points.size()*4);
    const auto writeImage = [&](const char* name, const cv::Mat& value) {
        std::ofstream file(out / name, std::ios::binary);
        file << "P5\n640 480\n255\n";
        file.write(reinterpret_cast<const char*>(value.data), value.total());
    };
    writeImage("first.pgm", image);
    cv::Mat relit;
    image.convertTo(relit, CV_8U, .8, 20);
    writeImage("second.pgm", relit);
    writeImage("blank.pgm", cv::Mat(image.size(), CV_8U, cv::Scalar(105)));
    std::ofstream metadata(out / "fixture.json");
    metadata << "{\"origin\":\"simulated\",\"generator\":\"World localization adapter synthetic texture and depth\",\"landmarks\":" << count
             << ",\"lens\":[570,570,320,240,0,0,0,0,0],\"world_from_camera\":[1,0,0,0,0,-1,0,0,0,0,-1,0,1,1.5,2,1]}\n";
    std::cout << count << " simulated landmarks generated\n";
}

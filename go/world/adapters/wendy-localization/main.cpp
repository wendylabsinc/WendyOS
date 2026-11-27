#include "SiteLocalization.h"
#include <opencv2/core.hpp>
#include <array>
#include <cmath>
#include <cstring>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <limits>
#include <map>
#include <memory>
#include <sstream>
#include <stdexcept>
#include <string>
#include <vector>
#include <sys/stat.h>

namespace {
// The parent validates map provenance/model binding. This process only solves
// a bounded image against supplied landmarks using the existing Wendy solver.
std::vector<unsigned char> readFile(const std::string& path, size_t maximum) {
    struct stat info{};
    if (lstat(path.c_str(), &info) || !S_ISREG(info.st_mode) || info.st_size < 0 ||
        uint64_t(info.st_size) > maximum) throw std::runtime_error("invalid input file");
    std::ifstream stream(path, std::ios::binary);
    std::vector<unsigned char> data(size_t(info.st_size));
    if (!stream || !stream.read(reinterpret_cast<char*>(data.data()), data.size()) ||
        stream.peek() != std::char_traits<char>::eof()) throw std::runtime_error("input file changed or could not be read");
    return data;
}

struct Image { int width, height; std::vector<unsigned char> pixels; };
Image readPGM(const std::string& path) {
    auto bytes = readFile(path, 32000000 + 64);
    size_t end = 0;
    std::array<std::string, 3> lines;
    for (auto& line : lines) {
        size_t start = end;
        while (end < bytes.size() && bytes[end] != '\n' && end < 64) ++end;
        if (end >= bytes.size() || end >= 64) throw std::runtime_error("invalid PGM header");
        line.assign(reinterpret_cast<char*>(bytes.data() + start), end - start);
        ++end;
    }
    Image image{};
    std::istringstream dimensions(lines[1]);
    std::string extra;
    if (lines[0] != "P5" || lines[2] != "255" || !(dimensions >> image.width >> image.height) ||
        dimensions >> extra || image.width < 32 || image.height < 32 || image.width > 8192 || image.height > 8192 ||
        int64_t(image.width) * image.height > 32000000 || bytes.size() - end != size_t(image.width) * image.height)
        throw std::runtime_error("invalid PGM image");
    image.pixels.assign(bytes.begin() + end, bytes.end());
    return image;
}

std::vector<float> readLandmarks(const std::string& path) {
    static_assert(sizeof(float) == 4 && std::numeric_limits<float>::is_iec559);
    auto bytes = readFile(path, size_t(WS_MAX_LANDMARKS) * WS_LANDMARK_FLOATS * 4);
    const size_t stride = WS_LANDMARK_FLOATS * 4;
    if (bytes.size() % stride || bytes.size() / stride < 120) throw std::runtime_error("invalid landmark count");
    std::vector<float> result(bytes.size() / 4);
    for (size_t i = 0; i < result.size(); ++i) {
        uint32_t bits = uint32_t(bytes[i*4]) | uint32_t(bytes[i*4+1]) << 8 |
                        uint32_t(bytes[i*4+2]) << 16 | uint32_t(bytes[i*4+3]) << 24;
        std::memcpy(&result[i], &bits, 4);
    }
    return result;
}

std::array<double, 9> readLens(const std::string& text) {
    std::array<double, 9> values{};
    std::istringstream input(text);
    for (size_t i = 0; i < values.size(); ++i) {
        std::string token;
        if (!std::getline(input, token, ',') || token.empty()) throw std::runtime_error("invalid lens");
        size_t used = 0;
        values[i] = std::stod(token, &used);
        if (used != token.size() || !std::isfinite(values[i])) throw std::runtime_error("invalid lens");
    }
    if (input.peek() != std::char_traits<char>::eof() || text.back() == ',' || values[0] <= 0 || values[1] <= 0)
        throw std::runtime_error("invalid lens");
    return values;
}

void emit(const WSLocalization& result, const std::string& mode) {
    for (double value : {result.fx, result.fy, result.cx, result.cy, result.rms})
        if (!std::isfinite(value)) throw std::runtime_error("nonfinite solver result");
    for (double value : result.distortion) if (!std::isfinite(value)) throw std::runtime_error("nonfinite solver result");
    for (double value : result.world_from_camera) if (!std::isfinite(value)) throw std::runtime_error("nonfinite solver result");
    std::cout << std::setprecision(17)
              << "{\"schema\":\"wendy.site-localization.result.v1\",\"success\":" << (result.success ? "true" : "false")
              << ",\"mode\":\"" << mode << "\",\"lens_assumed\":" << ((mode == "assumed" || result.fitted_lens == 2) ? "true" : "false")
              << ",\"reason\":" << result.reason << ",\"matches\":" << result.matches << ",\"inliers\":" << result.inliers
              << ",\"fitted_lens\":" << result.fitted_lens << ",\"fx\":" << result.fx << ",\"fy\":" << result.fy
              << ",\"cx\":" << result.cx << ",\"cy\":" << result.cy << ",\"rms\":" << result.rms << ",\"distortion\":[";
    for (int i = 0; i < 5; ++i) std::cout << (i ? "," : "") << result.distortion[i];
    std::cout << "],\"world_from_camera\":[";
    for (int i = 0; i < 16; ++i) std::cout << (i ? "," : "") << result.world_from_camera[i];
    std::cout << "]}\n";
}
}

int main(int argc, char** argv) {
    try {
        std::map<std::string, std::string> args;
        for (int i = 1; i < argc; i += 2) {
            std::string key = argv[i];
            if (i+1 >= argc || (key != "--landmarks" && key != "--image" && key != "--mode" && key != "--lens") ||
                !args.emplace(key, argv[i+1]).second) throw std::runtime_error("invalid arguments");
        }
        if (!args.count("--landmarks") || !args.count("--image") || !args.count("--mode"))
            throw std::runtime_error("required arguments: --landmarks FILE --image PGM --mode joint|measured|estimated|assumed");
        const auto mode = args.at("--mode");
        if (mode != "joint" && mode != "measured" && mode != "estimated" && mode != "assumed") throw std::runtime_error("invalid mode");
        const bool needsLens = mode == "measured" || mode == "assumed";
        if (needsLens != bool(args.count("--lens"))) throw std::runtime_error("lens required only for measured or assumed mode");
        std::array<double, 9> lens{};
        if (needsLens) lens = readLens(args.at("--lens"));
        auto landmarks = readLandmarks(args.at("--landmarks"));
        auto image = readPGM(args.at("--image"));
        cv::setNumThreads(2);
        std::unique_ptr<void, decltype(&WSMapDestroy)> map(WSMapCreate(), WSMapDestroy);
        const int count = int(landmarks.size() / WS_LANDMARK_FLOATS);
        if (!map || WSMapImport(map.get(), landmarks.data(), count) != count) throw std::runtime_error("invalid landmarks");
        WSLocalization result{};
        if (mode == "estimated") result = WSLocalizeEstimated(map.get(), image.pixels.data(), image.width, image.height);
        else if (mode == "assumed") result = WSLocalizeWithAssumedLens(map.get(), image.pixels.data(), image.width, image.height, lens.data());
        else result = WSLocalize(map.get(), image.pixels.data(), image.width, image.height, needsLens ? lens.data() : nullptr);
        emit(result, mode);
        return 0;
    } catch (...) {
        // Do not reflect local paths, image content or dependency exception text.
        std::cout << "{\"schema\":\"wendy.site-localization.result.v1\",\"success\":false,\"error\":\"invalid_input_or_solver_failure\"}\n";
        return 2;
    }
}

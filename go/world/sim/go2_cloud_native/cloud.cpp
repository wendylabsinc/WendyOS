// Passive private ROS observation only. No publisher, service, arm or HTTP client.
// Payloads are reserialized original ROS messages, not original transport packets.
#include <rclcpp/rclcpp.hpp>
#include <rclcpp/serialization.hpp>
#include <nav_msgs/msg/odometry.hpp>
#include <sensor_msgs/msg/laser_scan.hpp>
#include <sensor_msgs/msg/point_cloud2.hpp>
#include <std_msgs/msg/u_int64.hpp>
#include <tf2_msgs/msg/tf_message.hpp>
#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <iomanip>
#include <iostream>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

namespace {
constexpr size_t kRowBytes = 256 * 1024;
#if defined(WORLD_GO2_CLOUD_LIVE) && WORLD_GO2_CLOUD_LIVE == 1
// Separate, finite mission-input process. Its owner treats all completion as
// input loss until deliberate terminal cleanup; it never restarts this window.
constexpr size_t kRawBytes = 256 * 1024 * 1024;
constexpr size_t kMessages = 32768;
constexpr unsigned kMaxDurationMs = 60000;
constexpr unsigned kMinDurationMs = 1;
constexpr unsigned kMaxClouds = 2048;
constexpr const char *kNodeName = "world_go2_live_cloud_input";
#elif defined(WORLD_GO2_CLOUD_SHADOW) && WORLD_GO2_CLOUD_SHADOW == 1
// A separate passive executable. The original observer's limits below remain
// unchanged; there is no runtime flag that promotes its capture budget.
constexpr size_t kRawBytes = 16 * 1024 * 1024;
constexpr size_t kMessages = 8192;
constexpr unsigned kMaxDurationMs = 60000;
constexpr unsigned kMinDurationMs = 100;
constexpr unsigned kMaxClouds = 128;
constexpr const char *kNodeName = "world_go2_passive_cloud_shadow";
#else
constexpr size_t kRawBytes = 1024 * 1024;
constexpr size_t kMessages = 2048;
constexpr unsigned kMaxDurationMs = 15000;
constexpr unsigned kMinDurationMs = 100;
constexpr unsigned kMaxClouds = 5;
constexpr const char *kNodeName = "world_go2_passive_cloud_observer";
#endif
using Clock = std::chrono::steady_clock;

unsigned bounded(const char *s, unsigned low, unsigned high) {
  std::string value(s);
  if (value.empty() || value.size() > 5 || value.find_first_not_of("0123456789") != std::string::npos)
    throw std::runtime_error("invalid bound");
  const auto n = std::stoul(value);
  if (n < low || n > high) throw std::runtime_error("invalid bound");
  return unsigned(n);
}

// These checks bound reserialization work after ROS has already deserialized
// the message. Middleware allocation is not bounded by this callback; the
// passive app still needs the container's memory/PID limits.
void check(const sensor_msgs::msg::PointCloud2 &m) {
  if (m.header.frame_id.size() > 128 || m.height != 1 || m.width > 9000 ||
      m.fields.size() != 3 || m.point_step != 12 || m.row_step > 108000 ||
      m.data.size() > 108000) throw std::runtime_error("cloud shape bound");
  for (const auto &f : m.fields) if (f.name.size() > 32) throw std::runtime_error("cloud field bound");
}
void check(const sensor_msgs::msg::LaserScan &m) {
  if (m.header.frame_id.size() > 128 || m.ranges.size() > 8192 ||
      m.intensities.size() > 8192 || m.ranges.size()+m.intensities.size() > 8192)
    throw std::runtime_error("scan shape bound");
}
void check(const nav_msgs::msg::Odometry &m) {
  if (m.header.frame_id.size() > 128 || m.child_frame_id.size() > 128)
    throw std::runtime_error("odom string bound");
}
void check(const std_msgs::msg::UInt64 &) {}
void check(const tf2_msgs::msg::TFMessage &m) {
  if (m.transforms.size() > 32) throw std::runtime_error("transform count bound");
  for (const auto &t : m.transforms)
    if (t.header.frame_id.size() > 128 || t.child_frame_id.size() > 128)
      throw std::runtime_error("transform string bound");
}

class Observer : public rclcpp::Node {
public:
  Observer(unsigned max_clouds, Clock::time_point deadline) : Node(kNodeName, "/wendy_world"), max_clouds_(max_clouds), started_(Clock::now()), deadline_(deadline) {
    auto sensor = rclcpp::QoS(1).best_effort().durability_volatile();
    auto retained = rclcpp::QoS(1).reliable().transient_local();
    add<sensor_msgs::msg::PointCloud2>("/utlidar/cloud", sensor, 110000, true);
    add<sensor_msgs::msg::LaserScan>("/scan", sensor, 49152, false);
    add<nav_msgs::msg::Odometry>("/odom", sensor, 8192, false);
    add<std_msgs::msg::UInt64>("/simulation/epoch", retained, 64, false);
    add<tf2_msgs::msg::TFMessage>("/tf_static", retained, 16384, false);
  }
  bool done() const { return done_ || (draining_ && Clock::now() >= drain_deadline_); }
  void finish(const char *reason) {
    std::cout << "{\"kind\":\"complete\",\"reason\":\"" << reason
              << "\",\"messages\":" << messages_ << ",\"clouds\":" << clouds_
              << ",\"serialized_bytes\":" << raw_bytes_ << "}\n" << std::flush;
    if (!std::cout) throw std::runtime_error("output unavailable");
  }
  const char *reason() const { return reason_; }
private:
  std::vector<rclcpp::SubscriptionBase::SharedPtr> subscriptions_;
  unsigned max_clouds_;
  Clock::time_point started_;
  Clock::time_point deadline_, drain_deadline_;
  size_t messages_ = 0, raw_bytes_ = 0, clouds_ = 0;
  bool done_ = false;
  bool draining_ = false;
  const char *reason_ = "duration";
  template<class Message> void add(const char *topic, const rclcpp::QoS &qos, size_t maximum, bool cloud) {
    subscriptions_.push_back(create_subscription<Message>(topic, qos,
      [this, topic, maximum, cloud](typename Message::ConstSharedPtr message, const rclcpp::MessageInfo &info) {
        // Once the requested cloud count is reached, admit companions only.
        // A same-capture scan may be queued behind the final cloud. This does
        // not invent a join or extend the original absolute deadline.
        if (done() || Clock::now() >= deadline_ || (cloud && draining_)) return;
        try {
          if (!message) throw std::runtime_error("missing message");
          check(*message);
          rclcpp::Serialization<Message> serializer;
          rclcpp::SerializedMessage serialized;
          serializer.serialize_message(message.get(), &serialized);
          const auto &data = serialized.get_rcl_serialized_message();
          if (data.buffer_length == 0 || data.buffer_length > maximum) throw std::runtime_error("message bound");
          if (messages_ >= kMessages || data.buffer_length > kRawBytes - raw_bytes_) {
            done_ = true; reason_ = "capture_limit"; return;
          }
          const auto &metadata = info.get_rmw_message_info();
          std::ostringstream out;
          out << "{\"kind\":\"sample\",\"topic\":\"" << topic << "\",\"publisher_gid\":\"";
          out << std::hex << std::setfill('0');
          for (auto b : metadata.publisher_gid.data) out << std::setw(2) << unsigned(b);
          out << "\",\"source_timestamp_ns\":" << std::dec << metadata.source_timestamp;
          out << ",\"received_timestamp_ns\":" << metadata.received_timestamp;
          out << ",\"helper_elapsed_ns\":" << std::chrono::duration_cast<std::chrono::nanoseconds>(Clock::now()-started_).count();
          out << ",\"payload_hex\":\"" << std::hex;
          for (size_t i=0; i<data.buffer_length; ++i) out << std::setw(2) << unsigned(data.buffer[i]);
          out << "\"}\n";
          const auto line = out.str();
          if (line.size() > kRowBytes) throw std::runtime_error("row bound");
          // Serialization/formatting may cross a boundary. Do not knowingly
          // start a new output after either deadline. A blocked write still
          // requires the independent parent watchdog.
          if (done() || Clock::now() >= deadline_) return;
          std::cout << line << std::flush;
          if (!std::cout) throw std::runtime_error("output unavailable");
          ++messages_; raw_bytes_ += data.buffer_length;
          if (cloud && ++clouds_ >= max_clouds_) {
            draining_ = true;
            drain_deadline_ = std::min(deadline_, Clock::now() + std::chrono::milliseconds(100));
            reason_ = "max_clouds";
          }
        } catch (...) {
          std::cerr << "passive_cloud_helper_refused\n";
          std::_Exit(2);
        }
      }));
  }
};
}

int main(int argc, char **argv) {
  try {
    if (argc != 5 || std::string(argv[1]) != "--duration-ms" || std::string(argv[3]) != "--max-clouds") throw std::runtime_error("arguments");
    const auto duration = bounded(argv[2], kMinDurationMs, kMaxDurationMs);
    const auto max_clouds = bounded(argv[4], 1, kMaxClouds);
    // Includes ROS initialization; neither companion draining nor queued
    // callbacks are allowed to start new captures after this deadline.
    const auto deadline = Clock::now() + std::chrono::milliseconds(duration);
    rclcpp::init(0, nullptr);
    auto observer = std::make_shared<Observer>(max_clouds, deadline);
    rclcpp::executors::SingleThreadedExecutor executor;
    executor.add_node(observer);
    while (rclcpp::ok() && !observer->done() && Clock::now() < deadline) {
      executor.spin_some(std::chrono::milliseconds(5));
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
    observer->finish(observer->done() ? observer->reason() : (rclcpp::ok() ? "duration" : "interrupted"));
    rclcpp::shutdown();
    return 0;
  } catch (...) {
    std::cerr << "passive_cloud_helper_unavailable\n";
    return 1;
  }
}

// Private simulation sensor observer. Public rclcpp interfaces only.
// Carries actual message GID and serialized ROS data over this child stdout.
#include <rclcpp/rclcpp.hpp>
#include <rclcpp/serialization.hpp>
#include <nav_msgs/msg/odometry.hpp>
#include <sensor_msgs/msg/laser_scan.hpp>
#include <std_msgs/msg/u_int64.hpp>
#include <tf2_msgs/msg/tf_message.hpp>
#include <iostream>
#include <iomanip>
#include <sstream>
#include <vector>
#include <cstdlib>

class Observer : public rclcpp::Node {
public:
  Observer() : Node("world_go2_sensor_observer", "/wendy_world") {
    auto sensor = rclcpp::QoS(1).best_effort().durability_volatile();
    auto retained = rclcpp::QoS(1).reliable().transient_local();
    add<nav_msgs::msg::Odometry>("/odom", sensor);
    add<sensor_msgs::msg::LaserScan>("/scan", sensor);
    add<std_msgs::msg::UInt64>("/simulation/epoch", retained);
    add<tf2_msgs::msg::TFMessage>("/tf_static", retained);
  }
private:
  std::vector<rclcpp::SubscriptionBase::SharedPtr> subscriptions_;
  template<class Message> void add(const char *topic, const rclcpp::QoS &qos) {
    subscriptions_.push_back(create_subscription<Message>(topic, qos,
      [topic](typename Message::ConstSharedPtr msg, const rclcpp::MessageInfo &info) {
        try {
          rclcpp::Serialization<Message> serializer;
          rclcpp::SerializedMessage serialized;
          serializer.serialize_message(msg.get(), &serialized);
          const auto &data = serialized.get_rcl_serialized_message();
          if (data.buffer_length == 0 || data.buffer_length > 49152) {
            throw std::runtime_error("bounded sensor message exceeded");
          }
          const auto &metadata = info.get_rmw_message_info();
          std::ostringstream out;
          out << "{\"topic\":\"" << topic << "\",\"publisher_gid\":\"";
          out << std::hex << std::setfill('0');
          for (auto value : metadata.publisher_gid.data) out << std::setw(2) << unsigned(value);
          out << "\",\"source_timestamp_ns\":" << std::dec << metadata.source_timestamp;
          out << ",\"payload_hex\":\"" << std::hex;
          for (size_t i=0; i<data.buffer_length; ++i) out << std::setw(2) << unsigned(data.buffer[i]);
          out << "\"}\n";
          const auto line = out.str();
          if (line.size()>100000) throw std::runtime_error("bounded output exceeded");
          std::cout << line << std::flush;
          if (!std::cout) throw std::runtime_error("sensor pipe unavailable");
        } catch (...) {
          // Lost provenance or a broken private pipe must not silently retain
          // apparently live observations. Parent observes EOF and latches.
          std::cerr << "world sensor observer refused\n";
          std::_Exit(2);
        }
      }));
  }
};
int main(int argc, char **argv) {
  try {
    rclcpp::init(argc, argv);
    rclcpp::spin(std::make_shared<Observer>());
    rclcpp::shutdown();
    return 0;
  } catch (...) {
    std::cerr << "world sensor observer unavailable\n";
    return 1;
  }
}

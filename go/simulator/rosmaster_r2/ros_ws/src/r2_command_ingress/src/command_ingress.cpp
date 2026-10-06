// Preserve the DDS publisher identity and timestamp that Humble rclpy omits.
#include <cmath>
#include <iomanip>
#include <iostream>
#include <memory>
#include <sstream>

#include <geometry_msgs/msg/twist.hpp>
#include <rclcpp/rclcpp.hpp>

int main(int argc, char **argv) {
  rclcpp::init(argc, argv);
  auto node = std::make_shared<rclcpp::Node>("rosmaster_r2_command_ingress");
  auto subscription = node->create_subscription<geometry_msgs::msg::Twist>(
      "/cmd_vel", rclcpp::QoS(1).best_effort().durability_volatile(),
      [](geometry_msgs::msg::Twist::ConstSharedPtr message,
         const rclcpp::MessageInfo &info) {
        const auto &metadata = info.get_rmw_message_info();
        if (!std::isfinite(message->linear.x) || !std::isfinite(message->linear.y) ||
            !std::isfinite(message->angular.z)) {
          return;
        }
        std::ostringstream gid;
        for (auto byte : metadata.publisher_gid.data) {
          gid << std::hex << std::setfill('0') << std::setw(2)
              << static_cast<unsigned>(byte);
        }
        std::cout << std::setprecision(17)
                  << "{\"publisher_gid\":\"" << gid.str()
                  << "\",\"source_timestamp\":" << metadata.source_timestamp
                  << ",\"vx\":" << message->linear.x
                  << ",\"vy\":" << message->linear.y
                  << ",\"wz\":" << message->angular.z << "}" << std::endl;
      });
  rclcpp::spin(node);
  rclcpp::shutdown();
  return 0;
}

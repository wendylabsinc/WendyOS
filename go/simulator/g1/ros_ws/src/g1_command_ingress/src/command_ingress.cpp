#include <geometry_msgs/msg/twist.hpp>
#include <rclcpp/rclcpp.hpp>
#include <rclcpp/serialization.hpp>
#include <rmw/types.h>
#include <unitree_api/msg/request.hpp>
#include <unitree_hg/msg/low_cmd.hpp>

#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#include <array>
#include <cerrno>
#include <chrono>
#include <cmath>
#include <cstddef>
#include <cstdlib>
#include <cstring>
#include <iomanip>
#include <iostream>
#include <locale>
#include <memory>
#include <sstream>
#include <stdexcept>
#include <string>
#include <type_traits>

namespace
{
constexpr std::size_t kMaximumDatagramBytes = 4096;
// Reserve room for JSON keys, 24-byte GID, kind, and both int64 timestamps.
// Native CDR is hex encoded, so each serialized byte needs two JSON bytes.
constexpr std::size_t kMaximumSerializedBytes = (kMaximumDatagramBytes - 256) / 2;
constexpr char kDefaultSocket[] = "/run/wendy-g1/commands.sock";

class DatagramSink
{
public:
  DatagramSink()
  {
    const char * configured = std::getenv("G1_COMMAND_SOCKET");
    path_ = configured == nullptr ? kDefaultSocket : configured;
    if (path_.empty() || path_.front() != '/' || path_.size() >= sizeof(address_.sun_path)) {
      throw std::invalid_argument("G1_COMMAND_SOCKET must be an absolute filesystem path shorter "
        "than the Unix socket path limit");
    }
    address_.sun_family = AF_UNIX;
    std::memcpy(address_.sun_path, path_.c_str(), path_.size() + 1);
    address_size_ = static_cast<socklen_t>(offsetof(sockaddr_un, sun_path) + path_.size() + 1);
    descriptor_ = ::socket(AF_UNIX, SOCK_DGRAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
    if (descriptor_ < 0) {
      throw std::runtime_error(std::string("creating command datagram socket: ") + std::strerror(errno));
    }
  }

  ~DatagramSink() {::close(descriptor_);}
  DatagramSink(const DatagramSink &) = delete;
  DatagramSink & operator=(const DatagramSink &) = delete;

  const std::string & path() const {return path_;}

  bool send(const std::string & message, int & error) const
  {
    if (message.size() > kMaximumDatagramBytes) {
      error = EMSGSIZE;
      return false;
    }
    // Resolve the receiver pathname on each send. The runtime owns and may
    // recreate its socket on reset; no connected stale peer or retry queue is
    // retained here. A full/missing receiver drops this sample immediately.
    const ssize_t sent = ::sendto(descriptor_, message.data(), message.size(), MSG_DONTWAIT | MSG_NOSIGNAL,
      reinterpret_cast<const sockaddr *>(&address_), address_size_);
    if (sent < 0) {
      error = errno;
      return false;
    }
    if (static_cast<std::size_t>(sent) != message.size()) {
      error = EIO;
      return false;
    }
    return true;
  }

private:
  int descriptor_{-1};
  sockaddr_un address_{};
  socklen_t address_size_{};
  std::string path_;
};

std::string bytes_hex(const uint8_t * bytes, std::size_t size)
{
  static constexpr char digits[] = "0123456789abcdef";
  std::string result;
  result.reserve(size * 2);
  for (std::size_t index = 0; index < size; ++index) {
    const auto byte = bytes[index];
    result.push_back(digits[byte >> 4]);
    result.push_back(digits[byte & 0x0f]);
  }
  return result;
}

class CommandIngress final : public rclcpp::Node
{
public:
  CommandIngress() : Node("g1_command_ingress")
  {
    const auto qos = rclcpp::QoS(rclcpp::KeepLast(1)).best_effort().durability_volatile();
    subscription_ = create_subscription<geometry_msgs::msg::Twist>("/cmd_vel", qos,
      [this](geometry_msgs::msg::Twist::ConstSharedPtr message, const rclcpp::MessageInfo & info) {
        receive(*message, info);
      });
    sport_subscription_ = create_subscription<unitree_api::msg::Request>("/api/sport/request", qos,
      [this](unitree_api::msg::Request::ConstSharedPtr message, const rclcpp::MessageInfo & info) {
        receive_native("sport", *message, info);
      });
    motion_subscription_ = create_subscription<unitree_api::msg::Request>("/api/motion_switcher/request", qos,
      [this](unitree_api::msg::Request::ConstSharedPtr message, const rclcpp::MessageInfo & info) {
        receive_native("motion_switcher", *message, info);
      });
    lowcmd_subscription_ = create_subscription<unitree_hg::msg::LowCmd>("/lowcmd", qos,
      [this](unitree_hg::msg::LowCmd::ConstSharedPtr message, const rclcpp::MessageInfo & info) {
        receive_native("lowcmd", *message, info);
      });
    RCLCPP_INFO(get_logger(), "Forwarding attributed Twist and Unitree commands to %s; runtime admission is required",
      sink_.path().c_str());
  }

private:
  void receive(const geometry_msgs::msg::Twist & message, const rclcpp::MessageInfo & info)
  {
    const auto received = std::chrono::duration_cast<std::chrono::nanoseconds>(
      std::chrono::steady_clock::now().time_since_epoch()).count();
    const std::array<double, 6> values{message.linear.x, message.linear.y, message.linear.z,
      message.angular.x, message.angular.y, message.angular.z};
    for (const double value : values) {
      if (!std::isfinite(value)) {
        RCLCPP_WARN_THROTTLE(get_logger(), throttle_clock_, 2000, "Dropping nonfinite Twist");
        return;
      }
    }
    if (message.linear.z != 0.0 || message.angular.x != 0.0 || message.angular.y != 0.0) {
      RCLCPP_WARN_THROTTLE(get_logger(), throttle_clock_, 2000, "Dropping Twist with unsupported nonplanar axes");
      return;
    }
    const auto & metadata = info.get_rmw_message_info();
    std::ostringstream envelope;
    envelope.imbue(std::locale::classic());
    envelope << std::setprecision(17)
             << "{\"kind\":\"twist\",\"publisher_gid\":\""
             << bytes_hex(metadata.publisher_gid.data, RMW_GID_STORAGE_SIZE)
             << "\",\"source_timestamp_ns\":" << metadata.source_timestamp
             << ",\"received_ns\":" << received
             << ",\"velocity\":[" << message.linear.x << ',' << message.linear.y << ','
             << message.angular.z << "]}";
    send(envelope.str());
  }

  template<typename Message>
  void receive_native(const char * kind, const Message & message, const rclcpp::MessageInfo & info)
  {
    const auto received = std::chrono::duration_cast<std::chrono::nanoseconds>(
      std::chrono::steady_clock::now().time_since_epoch()).count();
    if constexpr (std::is_same_v<Message, unitree_api::msg::Request>) {
      // Reject unbounded strings/sequences before allocating a serialized copy.
      if (message.parameter.size() > kMaximumSerializedBytes ||
        message.binary.size() > kMaximumSerializedBytes ||
        message.parameter.size() + message.binary.size() > kMaximumSerializedBytes)
      {
        RCLCPP_WARN_THROTTLE(get_logger(), throttle_clock_, 2000,
          "Dropping oversized native command payload");
        return;
      }
    }
    try {
      rclcpp::Serialization<Message> serialization;
      rclcpp::SerializedMessage serialized;
      serialization.serialize_message(&message, &serialized);
      const auto & cdr = serialized.get_rcl_serialized_message();
      if (cdr.buffer_length > kMaximumSerializedBytes) {
        RCLCPP_WARN_THROTTLE(get_logger(), throttle_clock_, 2000,
          "Dropping oversized native command payload");
        return;
      }
      const auto & metadata = info.get_rmw_message_info();
      std::ostringstream envelope;
      envelope.imbue(std::locale::classic());
      envelope << "{\"kind\":\"" << kind << "\",\"publisher_gid\":\""
               << bytes_hex(metadata.publisher_gid.data, RMW_GID_STORAGE_SIZE)
               << "\",\"source_timestamp_ns\":" << metadata.source_timestamp
               << ",\"received_ns\":" << received << ",\"payload_hex\":\""
               << bytes_hex(cdr.buffer, cdr.buffer_length) << "\"}";
      send(envelope.str());
    } catch (const std::exception & error) {
      RCLCPP_WARN_THROTTLE(get_logger(), throttle_clock_, 2000,
        "Dropping unserializable native command: %s", error.what());
    }
  }

  void send(const std::string & envelope)
  {
    int error = 0;
    if (!sink_.send(envelope, error)) {
      RCLCPP_WARN_THROTTLE(get_logger(), throttle_clock_, 2000,
        "Dropping command datagram to %s: %s", sink_.path().c_str(), std::strerror(error));
    }
  }

  DatagramSink sink_;
  rclcpp::Clock throttle_clock_{RCL_STEADY_TIME};
  rclcpp::Subscription<geometry_msgs::msg::Twist>::SharedPtr subscription_;
  rclcpp::Subscription<unitree_api::msg::Request>::SharedPtr sport_subscription_;
  rclcpp::Subscription<unitree_api::msg::Request>::SharedPtr motion_subscription_;
  rclcpp::Subscription<unitree_hg::msg::LowCmd>::SharedPtr lowcmd_subscription_;
};
}  // namespace

int main(int argc, char * argv[])
{
  rclcpp::init(argc, argv);
  int result = 0;
  try {
    rclcpp::spin(std::make_shared<CommandIngress>());
  } catch (const std::exception & error) {
    std::cerr << "g1_command_ingress: " << error.what() << '\n';
    result = 1;
  }
  rclcpp::shutdown();
  return result;
}

#include <geometry_msgs/msg/twist.hpp>
#include <dds/dds.h>
#include <rclcpp/rclcpp.hpp>
#include <rclcpp/serialization.hpp>
#include <rmw/types.h>
#include <unitree_api/msg/request.hpp>
#include <unitree_go/msg/low_cmd.hpp>

#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#include <array>
#include <algorithm>
#include <cerrno>
#include <chrono>
#include <cmath>
#include <cstddef>
#include <cstdlib>
#include <cstring>
#include <iomanip>
#include <iostream>
#include <locale>
#include <map>
#include <memory>
#include <mutex>
#include <sstream>
#include <stdexcept>
#include <string>
#include <type_traits>
#include <vector>

namespace
{
constexpr std::size_t kMaximumDatagramBytes = 4096;
// Reserve room for JSON keys, 24-byte GID, kind, and both int64 timestamps.
// Native CDR is hex encoded, so each serialized byte needs two JSON bytes.
constexpr std::size_t kMaximumSerializedBytes = (kMaximumDatagramBytes - 256) / 2;
constexpr char kDefaultSocket[] = "/run/wendy-go2/commands.sock";

class DatagramSink
{
public:
  DatagramSink()
  {
    const char * configured = std::getenv("GO2_COMMAND_SOCKET");
    path_ = configured == nullptr ? kDefaultSocket : configured;
    if (path_.empty() || path_.front() != '/' || path_.size() >= sizeof(address_.sun_path)) {
      throw std::invalid_argument("GO2_COMMAND_SOCKET must be an absolute filesystem path shorter "
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

bool valid_node_name(const std::string & name)
{
  const auto first = [](char character) {
      return character == '_' || (character >= 'a' && character <= 'z') ||
             (character >= 'A' && character <= 'Z');
    };
  return !name.empty() && name.size() <= 128 && first(name.front()) &&
         std::all_of(name.begin(), name.end(), [&](char character) {
           return first(character) || (character >= '0' && character <= '9');
         });
}

bool valid_node_namespace(const std::string & value)
{
  if (value.empty() || value.front() != '/' || value.size() > 128) {
    return false;
  }
  if (value == "/") {
    return true;
  }
  std::size_t begin = 1;
  while (begin < value.size()) {
    const auto end = value.find('/', begin);
    if (!valid_node_name(value.substr(begin, end == std::string::npos ? end : end - begin))) {
      return false;
    }
    if (end == std::string::npos) {
      return true;
    }
    begin = end + 1;
  }
  return false;
}

class PublisherLabels
{
public:
  explicit PublisherLabels(rclcpp::Node & node) : node_(node) {}

  std::string lookup(const char * topic, const rmw_gid_t & identity)
  {
    const auto key = std::make_pair(std::string(topic),
      bytes_hex(identity.data, RMW_GID_STORAGE_SIZE));
    const std::lock_guard<std::mutex> lock(mutex_);
    auto cached = cache_.find(key);
    if (cached == cache_.end()) {
      if (cache_.size() >= 4096) {
        return {};
      }
      cached = cache_.emplace(key, Entry{}).first;
      cached->second.identity = identity;
    }
    return cached->second.fields;
  }

  void refresh()
  {
    // This timer has its own callback group and executor thread. The command
    // receive path only reads cached names; it never waits for DDS discovery.
    std::map<std::pair<std::string, std::string>, Entry> pending;
    {
      const std::lock_guard<std::mutex> lock(mutex_);
      pending = cache_;
    }
    std::map<std::string, std::vector<rclcpp::TopicEndpointInfo>> graphs;
    for (auto & [key, entry] : pending) {
      entry.fields.clear();
      try {
        auto graph = graphs.find(key.first);
        if (graph == graphs.end()) {
          graph = graphs.emplace(key.first, node_.get_publishers_info_by_topic(key.first)).first;
        }
        const auto & endpoints = graph->second;
        if (endpoints.size() <= 4096) {
          const auto & identity = entry.identity;
          std::array<uint8_t, RMW_GID_STORAGE_SIZE> graph_gid{};
          std::copy_n(identity.data, graph_gid.size(), graph_gid.begin());
          entry.fields = graph_label(endpoints, graph_gid);
          if (entry.fields.empty() && identity.implementation_identifier != nullptr &&
            std::strcmp(identity.implementation_identifier, "rmw_cyclonedds_cpp") == 0 &&
            cyclone_graph_gid(key.first.c_str(), identity, graph_gid))
          {
            entry.fields = graph_label(endpoints, graph_gid);
          }
        }
      } catch (const std::exception &) {
        // Graph changes and deleted DDS entities leave the source unnamed.
      }
      {
        const std::lock_guard<std::mutex> lock(mutex_);
        cache_.at(key).fields = std::move(entry.fields);
      }
    }
  }

private:
  struct Entry
  {
    rmw_gid_t identity{};
    std::string fields;
  };

  static std::string graph_label(const std::vector<rclcpp::TopicEndpointInfo> & endpoints,
    const std::array<uint8_t, RMW_GID_STORAGE_SIZE> & gid)
  {
    std::string name;
    std::string node_namespace;
    for (const auto & endpoint : endpoints) {
      if (endpoint.endpoint_gid() != gid) {
        continue;
      }
      if (!valid_node_name(endpoint.node_name()) ||
        !valid_node_namespace(endpoint.node_namespace()))
      {
        return {};
      }
      if (!name.empty() &&
        (name != endpoint.node_name() || node_namespace != endpoint.node_namespace()))
      {
        return {};  // Never pick a node from conflicting exact-identity records.
      }
      name = endpoint.node_name();
      node_namespace = endpoint.node_namespace();
    }
    if (name.empty()) {
      return {};
    }
    // Valid ROS names contain no JSON quotes, escapes or control characters.
    return ",\"node_name\":\"" + name + "\",\"node_namespace\":\"" + node_namespace + "\"";
  }

  bool cyclone_graph_gid(const char * topic, const rmw_gid_t & identity,
    std::array<uint8_t, RMW_GID_STORAGE_SIZE> & gid) const
  {
    // Humble Cyclone copies the native publication handle into message GIDs;
    // graph endpoints contain GUIDs instead (rmw_cyclonedds#377). Resolve that
    // handle through public DDS matched-writer data, never private RMW layouts.
    dds_instance_handle_t handle{};
    static_assert(sizeof(handle) <= RMW_GID_STORAGE_SIZE);
    if (!std::all_of(identity.data + sizeof(handle), identity.data + RMW_GID_STORAGE_SIZE,
      [](uint8_t value) {return value == 0;}))
    {
      return false;
    }
    std::memcpy(&handle, identity.data, sizeof(handle));
    if (handle == DDS_HANDLE_NIL) {
      return false;
    }
    constexpr std::size_t maximum_entities = 128;
    std::array<dds_entity_t, maximum_entities> entities{};
    const auto domain = node_.get_node_base_interface()->get_context()->get_domain_id();
    const auto participants = dds_lookup_participant(domain, entities.data(), entities.size());
    if (participants <= 0 || static_cast<std::size_t>(participants) > entities.size()) {
      return false;
    }
    std::size_t count = static_cast<std::size_t>(participants);
    const std::string dds_topic = "rt" + std::string(topic);
    for (std::size_t cursor = 0; cursor < count; ++cursor) {
      const auto entity = entities[cursor];
      const auto entity_topic = dds_get_topic(entity);
      std::array<char, 256> name{};
      if (entity_topic > 0 && dds_get_name(entity_topic, name.data(), name.size()) > 0 &&
        dds_topic == name.data())
      {
        const auto data = std::unique_ptr<dds_builtintopic_endpoint_t,
          decltype(&dds_builtintopic_free_endpoint)>(
          dds_get_matched_publication_data(entity, handle), dds_builtintopic_free_endpoint);
        if (data != nullptr) {
          gid.fill(0);
          static_assert(sizeof(data->key.v) <= RMW_GID_STORAGE_SIZE);
          std::copy_n(data->key.v, sizeof(data->key.v), gid.begin());
          return true;
        }
      }
      const auto children = dds_get_children(entity, entities.data() + count, entities.size() - count);
      if (children < 0) {
        continue;  // The entity may have disappeared during graph discovery.
      }
      if (static_cast<std::size_t>(children) > entities.size() - count) {
        return false;
      }
      count += static_cast<std::size_t>(children);
    }
    return false;
  }

  rclcpp::Node & node_;
  std::mutex mutex_;
  std::map<std::pair<std::string, std::string>, Entry> cache_;
};

class CommandIngress final : public rclcpp::Node
{
public:
  CommandIngress() : Node("go2_command_ingress"), labels_(*this)
  {
    const auto qos = rclcpp::QoS(rclcpp::KeepLast(1)).best_effort().durability_volatile();
    subscription_ = create_subscription<geometry_msgs::msg::Twist>("/cmd_vel", qos,
      [this](geometry_msgs::msg::Twist::ConstSharedPtr message, const rclcpp::MessageInfo & info) {
        receive(*message, info);
      });
    sport_subscription_ = create_subscription<unitree_api::msg::Request>("/api/sport/request", qos,
      [this](unitree_api::msg::Request::ConstSharedPtr message, const rclcpp::MessageInfo & info) {
        receive_native("sport", "/api/sport/request", *message, info);
      });
    motion_subscription_ = create_subscription<unitree_api::msg::Request>("/api/motion_switcher/request", qos,
      [this](unitree_api::msg::Request::ConstSharedPtr message, const rclcpp::MessageInfo & info) {
        receive_native("motion_switcher", "/api/motion_switcher/request", *message, info);
      });
    lowcmd_subscription_ = create_subscription<unitree_go::msg::LowCmd>("/lowcmd", qos,
      [this](unitree_go::msg::LowCmd::ConstSharedPtr message, const rclcpp::MessageInfo & info) {
        receive_native("lowcmd", "/lowcmd", *message, info);
      });
    metadata_group_ = create_callback_group(rclcpp::CallbackGroupType::MutuallyExclusive);
    metadata_timer_ = create_wall_timer(std::chrono::seconds(1),
      [this]() {labels_.refresh();}, metadata_group_);
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
    send(envelope.str(), labels_.lookup("/cmd_vel", metadata.publisher_gid));
  }

  template<typename Message>
  void receive_native(const char * kind, const char * topic, const Message & message,
    const rclcpp::MessageInfo & info)
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
      send(envelope.str(), labels_.lookup(topic, metadata.publisher_gid));
    } catch (const std::exception & error) {
      RCLCPP_WARN_THROTTLE(get_logger(), throttle_clock_, 2000,
        "Dropping unserializable native command: %s", error.what());
    }
  }

  void send(std::string envelope, const std::string & label_fields)
  {
    if (envelope.size() + label_fields.size() <= kMaximumDatagramBytes) {
      envelope.insert(envelope.size() - 1, label_fields);
    }
    int error = 0;
    if (!sink_.send(envelope, error)) {
      RCLCPP_WARN_THROTTLE(get_logger(), throttle_clock_, 2000,
        "Dropping command datagram to %s: %s", sink_.path().c_str(), std::strerror(error));
    }
  }

  DatagramSink sink_;
  PublisherLabels labels_;
  rclcpp::CallbackGroup::SharedPtr metadata_group_;
  rclcpp::TimerBase::SharedPtr metadata_timer_;
  rclcpp::Clock throttle_clock_{RCL_STEADY_TIME};
  rclcpp::Subscription<geometry_msgs::msg::Twist>::SharedPtr subscription_;
  rclcpp::Subscription<unitree_api::msg::Request>::SharedPtr sport_subscription_;
  rclcpp::Subscription<unitree_api::msg::Request>::SharedPtr motion_subscription_;
  rclcpp::Subscription<unitree_go::msg::LowCmd>::SharedPtr lowcmd_subscription_;
};
}  // namespace

int main(int argc, char * argv[])
{
  rclcpp::init(argc, argv);
  int result = 0;
  try {
    auto node = std::make_shared<CommandIngress>();
    rclcpp::executors::MultiThreadedExecutor executor(rclcpp::ExecutorOptions(), 2);
    executor.add_node(node);
    executor.spin();
  } catch (const std::exception & error) {
    std::cerr << "go2_command_ingress: " << error.what() << '\n';
    result = 1;
  }
  rclcpp::shutdown();
  return result;
}

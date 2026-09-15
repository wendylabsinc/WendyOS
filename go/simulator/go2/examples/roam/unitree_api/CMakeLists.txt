cmake_minimum_required(VERSION 3.8)
project(unitree_api)
find_package(ament_cmake REQUIRED)
find_package(rosidl_default_generators REQUIRED)
rosidl_generate_interfaces(${PROJECT_NAME}
  "msg/Request.msg"
  "msg/RequestHeader.msg"
  "msg/RequestIdentity.msg"
  "msg/RequestLease.msg"
  "msg/RequestPolicy.msg"
)
ament_export_dependencies(rosidl_default_runtime)
ament_package()

cmake_minimum_required(VERSION 3.16)
project(wendy_exact_goal_planner)

find_package(ament_cmake REQUIRED)
find_package(nav2_smac_planner REQUIRED)
find_package(nav2_core REQUIRED)
find_package(nav2_costmap_2d REQUIRED)
find_package(pluginlib REQUIRED)
find_package(nav_msgs REQUIRED)
find_package(geometry_msgs REQUIRED)
find_package(rclcpp REQUIRED)
find_package(rclcpp_lifecycle REQUIRED)
find_package(tf2_ros REQUIRED)

add_library(exact_goal_planner SHARED src/exact_goal_planner.cpp)
target_compile_features(exact_goal_planner PUBLIC cxx_std_17)
target_include_directories(exact_goal_planner PUBLIC
  $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
  $<INSTALL_INTERFACE:include>)
ament_target_dependencies(exact_goal_planner nav2_smac_planner nav2_core
  nav2_costmap_2d pluginlib nav_msgs geometry_msgs rclcpp rclcpp_lifecycle tf2_ros)
pluginlib_export_plugin_description_file(nav2_core plugin.xml)
install(TARGETS exact_goal_planner LIBRARY DESTINATION lib)
install(DIRECTORY include/ DESTINATION include)

if(BUILD_TESTING)
  enable_testing()
  add_executable(terminal_orientation_test test/terminal_orientation_test.cpp)
  target_compile_features(terminal_orientation_test PRIVATE cxx_std_17)
  target_include_directories(terminal_orientation_test PRIVATE include)
  add_test(NAME terminal_orientation_contract COMMAND terminal_orientation_test)
endif()

ament_package()

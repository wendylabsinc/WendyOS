cmake_minimum_required(VERSION 3.8)
project(g1_command_ingress)

find_package(ament_cmake REQUIRED)
find_package(rclcpp REQUIRED)
find_package(geometry_msgs REQUIRED)
find_package(rmw REQUIRED)
find_package(unitree_api REQUIRED)
find_package(unitree_hg REQUIRED)

add_executable(g1_command_ingress src/command_ingress.cpp)
target_compile_features(g1_command_ingress PUBLIC cxx_std_17)
target_compile_options(g1_command_ingress PRIVATE -Wall -Wextra -Wpedantic)
ament_target_dependencies(g1_command_ingress rclcpp geometry_msgs rmw unitree_api unitree_hg)
install(TARGETS g1_command_ingress DESTINATION lib/${PROJECT_NAME})

if(BUILD_TESTING)
  enable_testing()
  find_package(Python3 REQUIRED COMPONENTS Interpreter)
  add_test(
    NAME attributed_command_datagrams
    COMMAND ${Python3_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/test/integration_test.py
      $<TARGET_FILE:g1_command_ingress>)
  set_tests_properties(attributed_command_datagrams PROPERTIES TIMEOUT 40)
endif()

ament_package()

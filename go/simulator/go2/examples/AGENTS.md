# Go2 example constraints

- The user reports that the physical Unitree Go2 ignores forward speed requests
  below 0.5 m/s. Use 0.55 m/s as the default forward speed in every driving
  example, including Patrol, Roam and Teleop. Preserve that forward component
  when combining directions rather than normalizing it below the minimum.
- Zero velocity remains the stop command. This is a linear forward-speed
  limitation in m/s, not an angular turning-speed limitation in rad/s.

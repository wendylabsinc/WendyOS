#pragma once
#include <math.h>

// Heat below target - 0.5 °C, stop above target + 0.5 °C, so it doesn't toggle on noise.
const float HYSTERESIS = 0.5;

// The heating state after one reading. A missing reading (NaN) never heats.
inline bool nextHeating(bool heating, bool enabled, float celsius, int target) {
  if (!enabled || isnan(celsius)) return false;
  if (celsius < target - HYSTERESIS) return true;
  if (celsius > target + HYSTERESIS) return false;
  return heating;
}

// The room reached the target: heating stopped on its own, not by a switch-off or a lost sensor.
inline bool reachedTarget(bool wasHeating, bool heating, bool enabled, float celsius) {
  return wasHeating && !heating && enabled && !isnan(celsius);
}

// Host test for the sketch's control rules: g++ -std=c++17 test/control_test.cpp && ./a.out
#include <cassert>
#include <cmath>
#include <cstdio>
#include <initializer_list>
#include "../sketch/control.h"

int main() {
  // Starts heating only below target - 0.5, stops only above target + 0.5.
  assert(nextHeating(false, true, 20.4f, 21));
  assert(!nextHeating(false, true, 20.6f, 21));
  assert(nextHeating(true, true, 21.4f, 21));
  assert(!nextHeating(true, true, 21.6f, 21));
  // Off or no reading never heats.
  assert(!nextHeating(true, false, 15.0f, 21));
  assert(!nextHeating(true, true, NAN, 21));
  // The chirp: only when heating stopped because the room got warm.
  assert(reachedTarget(true, false, true, 21.6f));
  assert(!reachedTarget(true, false, false, 15.0f));
  assert(!reachedTarget(true, false, true, NAN));
  assert(!reachedTarget(false, false, true, 22.0f));
  // A full warm-up: idle in the band, heats as it cools, chirps once when warm.
  bool heating = false;
  int chirps = 0;
  for (float c : {21.0f, 20.6f, 20.4f, 20.8f, 21.3f, 21.6f, 21.2f, 20.6f}) {
    bool was = heating;
    heating = nextHeating(heating, true, c, 21);
    chirps += reachedTarget(was, heating, true, c);
  }
  assert(chirps == 1 && !heating);
  std::puts("control rules: ok");
}

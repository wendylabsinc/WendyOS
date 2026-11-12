// Thermostat, microcontroller half. The control loop runs here, so heating keeps
// following the target while Linux or the app restarts.
#include <Arduino_Modulino.h>
#include <Arduino_RouterBridge.h>
#include <ArduinoGraphics.h>
#include <Arduino_LED_Matrix.h>
#include "control.h"

ModulinoKnob knob;
ModulinoThermo thermo;
ModulinoBuzzer buzzer;
Arduino_LED_Matrix matrix;

const int TARGET_MIN = 10;
const int TARGET_MAX = 30;

int target = 21;
bool enabled = true;
bool heating = false;
float celsius = NAN;
float humidity = NAN;

int16_t lastKnob = 0;
bool lastPressed = false;
unsigned long lastRead = 0;
unsigned long lastNotify = 0;
bool notifyNow = true;
String message;
unsigned long messageUntil = 0;
String shown;

void showMessage(const String &text) {
  message = text;
  messageUntil = millis() + 2000;
}

int setTarget(int t) {
  target = constrain(t, TARGET_MIN, TARGET_MAX);
  showMessage("T" + String(target));
  notifyNow = true;
  return target;
}

bool setEnabled(bool on) {
  if (on != enabled) {
    enabled = on;
    buzzer.tone(on ? 880 : 440, 80);
    showMessage(on ? "ON" : "OFF");
  }
  notifyNow = true;
  return enabled;
}

// What the matrix shows, with "|" for the heating bar: lets the app check it remotely.
String matrixText() { return shown; }

void control() {
  bool wasHeating = heating;
  heating = nextHeating(heating, enabled, celsius, target);
  if (heating != wasHeating) {
    notifyNow = true;
  }
  if (reachedTarget(wasHeating, heating, enabled, celsius)) {
    buzzer.tone(1320, 150);
  }
}

// "21C" takes 12 of the 13 columns; the last one shows heating as a full-height bar.
void draw() {
  String text;
  if (millis() < messageUntil) {
    text = message;
  } else if (isnan(celsius)) {
    text = "--";
  } else {
    text = String((int)lround(celsius)) + "C";
  }
  String key = text + (heating ? "|" : "");
  if (key == shown) return;
  shown = key;
  matrix.beginDraw();
  matrix.clear();
  matrix.stroke(0xFFFFFFFF);
  matrix.textFont(Font_4x6);
  matrix.beginText(0, 1, 0xFFFFFF);
  matrix.print(text);
  matrix.endText();
  if (heating) {
    matrix.line(12, 0, 12, 7);
  }
  matrix.endDraw();
}

void setup() {
  matrix.begin();
  Bridge.begin();
  Modulino.begin();
  knob.begin();
  thermo.begin();
  buzzer.begin();
  lastKnob = knob.get();
  // provide_safe runs the handler in loop()'s context, so it never races the
  // I2C traffic below.
  Bridge.provide_safe("set_target", setTarget);
  Bridge.provide_safe("set_enabled", setEnabled);
  Bridge.provide_safe("matrix_text", matrixText);
}

void loop() {
  int16_t value = knob.get();
  if (value != lastKnob) {
    setTarget(target + (value - lastKnob));
    lastKnob = value;
  }
  bool pressed = knob.isPressed();
  if (pressed && !lastPressed) {
    setEnabled(!enabled);
  }
  lastPressed = pressed;

  if (millis() - lastRead >= 2000) {
    lastRead = millis();
    celsius = thermo.getTemperature();
    humidity = thermo.getHumidity();
  }
  control();

  // Sent every 2 s and on every change, so an app that starts later catches up.
  if (notifyNow || millis() - lastNotify >= 2000) {
    notifyNow = false;
    lastNotify = millis();
    Bridge.notify("state", celsius, humidity, target, enabled, heating);
  }
  draw();
  delay(50);
}

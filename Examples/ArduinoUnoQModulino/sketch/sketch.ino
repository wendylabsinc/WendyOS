// Modulino demo, microcontroller half: reads the Knob and Thermo on the Qwiic
// bus, shows the temperature on the LED matrix, and lets Linux beep the Buzzer
// through Arduino's Bridge.
#include <Arduino_Modulino.h>
#include <Arduino_RouterBridge.h>
#include <ArduinoGraphics.h>
#include <Arduino_LED_Matrix.h>

ModulinoKnob knob;
ModulinoThermo thermo;
ModulinoBuzzer buzzer;
Arduino_LED_Matrix matrix;

String shown = "";
int16_t lastKnob = INT16_MIN;
bool lastPressed = false;
unsigned long lastClimate = 0;

void buzz(int freq, int ms) { buzzer.tone(freq, ms); }
String matrix_text() { return shown; }

// Whole degrees fit the 13x8 matrix with the 4x6 font ("23C" is 12 columns);
// anything wider scrolls past once. No Thermo reads as NaN: show "--".
void showTemperature(float celsius) {
  int t = isnan(celsius) ? 0 : (int)lround(celsius);
  String text = isnan(celsius) ? String("--") : String(t) + "C";
  if (text == shown) return;
  shown = text;
  matrix.beginDraw();
  matrix.clear();
  matrix.stroke(0xFFFFFFFF);
  matrix.textFont(Font_4x6);
  if (isnan(celsius) || (t >= -9 && t <= 99)) {
    matrix.beginText(0, 1, 0xFFFFFF);
    matrix.print(text);
    matrix.endText();
  } else {
    matrix.textScrollSpeed(60);
    matrix.beginText(0, 1, 0xFFFFFF);
    matrix.print(" " + text + " ");
    matrix.endText(SCROLL_LEFT);
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
  // provide_safe runs the handler in loop()'s context, so it never races the
  // I2C traffic below.
  Bridge.provide_safe("buzz", buzz);
  Bridge.provide_safe("matrix_text", matrix_text);
}

void loop() {
  int16_t value = knob.get();
  bool pressed = knob.isPressed();
  bool periodic = millis() - lastClimate >= 2000;
  // Also resent every 2 s, so an app that starts after the sketch catches up.
  if (value != lastKnob || pressed != lastPressed || periodic) {
    lastKnob = value;
    lastPressed = pressed;
    Bridge.notify("knob", value, pressed);
  }

  if (periodic) {
    lastClimate = millis();
    float celsius = thermo.getTemperature();
    float humidity = thermo.getHumidity();
    showTemperature(celsius);
    Bridge.notify("climate", celsius, humidity);
  }
  delay(50);
}

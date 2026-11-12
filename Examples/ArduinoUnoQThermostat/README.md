# UNO Q Thermostat

A room thermostat on an Arduino UNO Q with the Modulino Knob, Thermo and Buzzer.

- **Turn the knob**: the target goes up or down 1 °C per step (10 to 30 °C). The matrix shows it, e.g. `T23`.
- **Press the knob**: turns the thermostat on or off (`ON` / `OFF` on the matrix, a high or low beep).
- **The matrix** shows the room temperature, e.g. `21C`. A bar in the right-most column means it is heating.
- **The buzzer** chirps when the room reaches the target.
- **The web page** on port 8000 shows room, target and humidity, sets the target and on/off, and charts the last 24 hours.

## Run

```sh
wendy run --device <your UNO Q>
```

`wendy run` compiles `sketch/` and flashes it to the microcontroller, then builds and starts the app.
Open `http://<board IP>:8000`. If another app on the board already uses port 8000, remove it first.

## How it is split

- **Sketch (microcontroller)**: reads the Knob and Thermo, runs the control loop (heat below target − 0.5 °C,
  stop above target + 0.5 °C), drives the matrix and buzzer. It keeps working while Linux or the app restarts.
- **App (Linux)**: mirrors the sketch's state through Arduino's Bridge, keeps 24 hours of history in memory, serves the page, and forwards
  target and on/off changes to the sketch.

## Notes

- "Heating" is a signal only; nothing is switched. A relay on a free pin would make it real.
- After the microcontroller restarts (power cut, new flash), the target goes back to 21 °C and the thermostat is on.
- The history starts empty when the app restarts.

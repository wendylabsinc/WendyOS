# Arduino UNO Q Modulino demo

One `wendy run` puts both halves of this app on an Arduino UNO Q:

- **`sketch/`** runs on the board's STM32. It reads a Modulino Knob and a Modulino
  Thermo on the Qwiic connector, shows the temperature on the LED matrix (for
  example `23C`), and drives a Modulino Buzzer.
- **`app.py`** runs in a container on WendyOS. It gets the knob and the
  temperature from the sketch through Arduino's Bridge and serves a page on
  port 8000 with the readings and a **Beep** button.

```sh
wendy run
```

`wendy run` compiles the sketch with a pinned `arduino-cli`, flashes it to the
microcontroller, then deploys the container. The board needs a WendyOS that can
flash its microcontroller; an older one stops the run and asks you to update.

## How the two halves talk

`wendy.json` names the sketch with `mcu` and grants `arduino-bridge`, which gives
the container Arduino's router socket and sets `APP_SOCKET`:

| Sketch (`Arduino_RouterBridge`) | App (`arduino-router-bridge`) |
|---|---|
| `Bridge.notify("knob", value, pressed)` | `bridge.provide("knob", ...)` |
| `Bridge.notify("climate", celsius, humidity)` | `bridge.provide("climate", ...)` |
| `Bridge.provide_safe("buzz", buzz)` | `bridge.call("buzz", 880, 200)` |
| `Bridge.provide_safe("matrix_text", ...)` | `bridge.call("matrix_text")` returns what the matrix shows |

`sketch/sketch.yaml` pins the Arduino core and every library, the same way an
Arduino App Lab app does, so the sketch builds the same everywhere.

## Tests

```sh
python3 -m unittest test_app.py
```

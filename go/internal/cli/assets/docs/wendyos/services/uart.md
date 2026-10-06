# Debug UART

WendyOS exposes a serial console on the primary UART for debugging, recovery, and interactive login. The UART is enabled by default on all supported hardware.

## Hardware connections

Connect a 3.3 V USB-to-serial adapter to GPIO pins 14 (TXD) and 15 (RXD) on the 40-pin header, plus a GND pin.

| RPi model | UART device | Notes |
|-----------|------------|-------|
| RPi 4 | `ttyS0` | Mini UART, derived from VPU clock. Baud rate may drift slightly under heavy CPU load. Bluetooth uses the full PL011 UART (`ttyAMA0`) on RPi 4. |
| RPi 5 | `ttyAMA0` (alias `ttyAMA10`) | Full PL011 UART; `uart0-pi5` overlay. RPi 5 has a dedicated UART for the serial console, separate from Bluetooth. |

The kernel and serial-getty use the `serial0` alias, which the RPi firmware maps to the correct device for the model:

| Model | `serial0` → |
|-------|-----------|
| RPi 4 | `ttyS0` |
| RPi 5 | `ttyAMA0` |

## Baud rate

**115200 baud, 8N1** (8 data bits, no parity, 1 stop bit).

This is set in two places:

1. Kernel command line: `console=serial0,115200` (assembled by meta-raspberrypi)
2. systemd-serialgetty: `SERIAL_CONSOLES = "115200;ttyS0"` (RPi 4) or `"115200;ttyAMA0"` (RPi 5)

## Enabling the UART

The UART is enabled at build time. In `raspberrypi5-wendyos.conf` / `raspberrypi4-64-wendyos.conf`:

```bitbake
ENABLE_UART = "1"
```

This causes `rpi-config_%.bbappend` to append the following to `config.txt`:

```ini
enable_uart=1

# RPi 4
dtoverlay=uart0

# RPi 5
dtoverlay=uart0-pi5
```

You can also enable it at runtime by editing `/boot/config.txt`:

```ini
enable_uart=1
dtoverlay=uart0       # RPi 4
# dtoverlay=uart0-pi5 # RPi 5
```

Then reboot.

## systemd serial-getty

A login shell is provided on the serial console by `serial-getty@<device>.service`. The unit is installed via `recipes-core/systemd/systemd-serialgetty.bbappend` and configured for **auto-login as root**:

```ini
[Service]
ExecStart=-/sbin/agetty -a root -8 -L %I 115200 $TERM
```

The `-a root` flag means the UART console drops straight to a root shell without requiring a password. This is intentional for a development/embedded platform — disable or change it for production deployments by overriding the unit:

```sh
# Create a drop-in to require a password
mkdir -p /etc/systemd/system/serial-getty@ttyS0.service.d/
cat > /etc/systemd/system/serial-getty@ttyS0.service.d/require-password.conf << 'EOF'
[Service]
ExecStart=
ExecStart=-/sbin/agetty -8 -L %I 115200 $TERM
EOF
systemctl daemon-reload
systemctl restart serial-getty@ttyS0.service
```

## Connecting from a host computer

```sh
# macOS (check /dev/tty.usbserial-* for your adapter)
screen /dev/tty.usbserial-XXXX 115200

# Linux
screen /dev/ttyUSB0 115200
# or
minicom -D /dev/ttyUSB0 -b 115200

# Using picocom
picocom -b 115200 /dev/ttyUSB0
```

Press `Ctrl-A K` (screen) or `Ctrl-A X` (minicom/picocom) to exit.

## ACM serial console over USB

The USB gadget also exposes an ACM serial function alongside the network. This creates:

- `/dev/ttyGS0` on the WendyOS device
- `ttyACMx` on the connected host (usually `ttyACM0`)

This is separate from the hardware UART and does not require any GPIO wiring. It is available whenever the USB gadget is active:

```sh
# On the host
screen /dev/ttyACM0 115200
```

On Linux, ModemManager may claim `ttyACM0` and block the connection. If `screen` fails to open the device, stop ModemManager. If the USB-C link hasn't been configured yet, `wendy discover` offers to set it up, and that setup also installs a udev rule that keeps ModemManager off the gadget.

The ACM device provides a shell session (via getty) on the device side at `/dev/ttyGS0`.

## Kernel boot messages

Early kernel boot messages appear on the serial console before systemd starts. To capture them, connect the serial adapter before powering on the device.

To disable the quiet kernel splash and see all boot messages, remove `quiet` and `splash` from `cmdline.txt` if present.

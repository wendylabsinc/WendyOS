# NVMe-specific image configuration for RPi5
# This file is included when building for the raspberrypi5-nvme machine
# (MACHINEOVERRIDES contains 'raspberrypi5-nvme')

IMAGE_INSTALL:append = " rpi-eeprom-nvme-config"

# Maps DEVICE + STORAGE to the Yocto MACHINE name.
# Require this file after setting DEVICE and STORAGE in your board's local.conf.

MACHINE = "${@{'raspberry-pi-3:sd': 'raspberrypi3-64-wendyos', 'raspberry-pi-4:sd': 'raspberrypi4-64-wendyos', 'raspberry-pi-5:sd': 'raspberrypi5-wendyos', 'raspberry-pi-5:nvme': 'raspberrypi5-nvme-wendyos', 'jetson-agx-orin:nvme': 'jetson-agx-orin-devkit-nvme-wendyos', 'jetson-orin-nano:nvme': 'jetson-orin-nano-devkit-nvme-wendyos', 'jetson-orin-nano:sd': 'jetson-orin-nano-devkit-wendyos'}.get((d.getVar('DEVICE') or '') + ':' + (d.getVar('STORAGE') or ''), '')}"

Downloads an image for [WendyOS](../../../../wendyos/) or [Wendy Lite](../../../../wendy-lite/) to your developer machine.

For Jetson boards that install over USB recovery (AGX Thor, and Orin Nano and AGX Orin on recovery releases), it downloads the flashpack instead, so a full USB flash with `wendy os install` needs no flashpack download. NVMe is the default; pass `--storage emmc` for the AGX Orin eMMC flashpack.

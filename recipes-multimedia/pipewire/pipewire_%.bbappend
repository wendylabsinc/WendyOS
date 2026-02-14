# Disable Vulkan support - not needed for audio and avoids mesa dependency
PACKAGECONFIG:remove = "vulkan"

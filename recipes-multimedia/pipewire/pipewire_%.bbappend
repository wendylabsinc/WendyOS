# Audio-only configuration - disable graphics/video features to avoid heavy dependencies
# This reduces build complexity and avoids mesa, vulkan, libcamera dependencies
PACKAGECONFIG:remove = "vulkan libcamera v4l2 sdl2 gstreamer"

# Keep only audio-essential features
# alsa: ALSA plugin for audio
# bluez: Bluetooth audio support
# systemd: System integration
# wireplumber: Session manager
# pulseaudio: PulseAudio compatibility layer

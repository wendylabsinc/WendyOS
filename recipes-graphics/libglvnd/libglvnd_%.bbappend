# Add virtual GL/EGL/GLES providers for wendyOS
# libglvnd with NVIDIA's tegra-libraries-*core provides full GL/EGL/GLES stack
# This resolves build dependency issues with tegra-libraries-multimedia packages

# Explicitly provide virtual interfaces when appropriate PACKAGECONFIG options are enabled
PROVIDES:append = " ${@bb.utils.contains('PACKAGECONFIG', 'egl', 'virtual/egl', '', d)}"
PROVIDES:append = " ${@bb.utils.contains('PACKAGECONFIG', 'gles1', 'virtual/libgles1', '', d)}"
PROVIDES:append = " ${@bb.utils.contains('PACKAGECONFIG', 'gles2', 'virtual/libgles2', '', d)}"

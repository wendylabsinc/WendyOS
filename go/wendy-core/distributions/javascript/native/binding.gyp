{
  "targets": [{
    "target_name": "wendy_core",
    "sources": ["addon.c"],
    "include_dirs": ["../../../include/WendyCore", "../../../internal/bridge"],
    "defines": ["NAPI_VERSION=8"],
    "conditions": [["OS=='linux'", {"libraries": ["-ldl"]}]]
  }]
}

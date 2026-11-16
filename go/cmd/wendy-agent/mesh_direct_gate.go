package main

import "os"

func meshDirectBLEEnabled() bool {
	value, present := os.LookupEnv("WENDY_NET_DIRECT_BLE")
	return !present || value == "1"
}

package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

//export WDSensorTemperature
func WDSensorTemperature(session C.uint64_t) *C.WDSensorTemperatureResult {
	value, err := desktop.SensorTemperature(uint64(session))
	p := alloc[C.WDSensorTemperatureResult](1)
	p.status = status(err)
	p.name = C.CString(value.Name)
	p.celsius = C.double(value.Celsius)
	p.timestamp_us = C.uint64_t(value.TimestampUs)
	p.sequence = C.uint32_t(value.Sequence)
	return p
}

//export WDSensorTemperatureFree
func WDSensorTemperatureFree(p *C.WDSensorTemperatureResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.name)
	free(p)
}

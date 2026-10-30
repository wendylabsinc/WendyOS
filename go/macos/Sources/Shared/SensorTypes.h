#ifndef WENDY_SENSOR_TYPES_H
#define WENDY_SENSOR_TYPES_H
typedef struct {
    WDStatus status;
    char *name;
    double celsius;
    uint64_t timestamp_us;
    uint32_t sequence;
} WDSensorTemperatureResult;
WDSensorTemperatureResult *WDSensorTemperature(uint64_t session);
void WDSensorTemperatureFree(WDSensorTemperatureResult *value);
#endif

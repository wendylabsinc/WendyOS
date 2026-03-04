#import <CoreBluetooth/CoreBluetooth.h>
#import <Foundation/Foundation.h>
#include <stdlib.h>
#include <string.h>
#include "bluetooth_darwin.h"

// WendyBLEScanner implements CBCentralManagerDelegate to discover BLE peripherals
// advertising the WendyOS service UUID.
@interface WendyBLEScanner : NSObject <CBCentralManagerDelegate>

@property (nonatomic, strong) CBCentralManager *centralManager;
@property (nonatomic, strong) dispatch_queue_t bleQueue;
@property (nonatomic, strong) NSMutableDictionary<NSUUID *, NSDictionary *> *discovered;
@property (nonatomic) dispatch_semaphore_t readySem;
@property (nonatomic) BOOL isReady;

@end

@implementation WendyBLEScanner

- (instancetype)init {
    self = [super init];
    if (self) {
        _discovered = [NSMutableDictionary new];
        _readySem = dispatch_semaphore_create(0);
        _isReady = NO;
        _bleQueue = dispatch_queue_create("com.wendylabs.ble.scan", DISPATCH_QUEUE_SERIAL);
        _centralManager = [[CBCentralManager alloc] initWithDelegate:self
                                                               queue:_bleQueue
                                                             options:nil];
    }
    return self;
}

#pragma mark - CBCentralManagerDelegate

- (void)centralManagerDidUpdateState:(CBCentralManager *)central {
    switch (central.state) {
        case CBManagerStatePoweredOn:
            self.isReady = YES;
            dispatch_semaphore_signal(self.readySem);
            break;
        case CBManagerStatePoweredOff:
        case CBManagerStateUnauthorized:
        case CBManagerStateUnsupported:
            // Bluetooth not usable — unblock the caller.
            dispatch_semaphore_signal(self.readySem);
            break;
        default:
            // Resetting / unknown — keep waiting.
            break;
    }
}

- (void)centralManager:(CBCentralManager *)central
 didDiscoverPeripheral:(CBPeripheral *)peripheral
     advertisementData:(NSDictionary<NSString *, id> *)advertisementData
                  RSSI:(NSNumber *)RSSI {

    NSUUID *peripheralID = peripheral.identifier;
    int rssi = [RSSI intValue];

    // Resolve the peripheral name early so we can use it for identification.
    NSString *name = advertisementData[CBAdvertisementDataLocalNameKey];
    if (!name || name.length == 0) {
        name = peripheral.name;
    }

    // Determine device type by inspecting advertised service UUIDs.
    CBUUID *agentUUID = [CBUUID UUIDWithString:@"7565E9EB-4C20-4B67-9272-D708B397B631"];
    CBUUID *liteUUID  = [CBUUID UUIDWithString:@"4E57454E-4459-0001-0000-000000000000"];
    NSArray<CBUUID *> *serviceUUIDs = advertisementData[CBAdvertisementDataServiceUUIDsKey];

    BOOL hasAgent = NO;
    BOOL hasLite  = NO;
    for (CBUUID *svc in serviceUUIDs) {
        if ([svc isEqual:agentUUID]) hasAgent = YES;
        if ([svc isEqual:liteUUID])  hasLite  = YES;
    }

    // Wendy Lite (ESP32) devices may not advertise their service UUID;
    // fall back to matching the "Wendy-" BLE name prefix.
    if (!hasAgent && !hasLite) {
        if (name && [name hasPrefix:@"Wendy-"]) {
            hasLite = YES;
        } else {
            return; // Not a Wendy device.
        }
    }

    // If the agent UUID is present, treat as full agent even if lite UUID also appears.
    BOOL isLite = (!hasAgent && hasLite);

    if (!name || name.length == 0) {
        name = isLite ? @"Wendy Lite" : @"WendyOS Device";
    }

    // Dedup: keep the entry with the strongest RSSI per peripheral UUID.
    NSDictionary *existing = self.discovered[peripheralID];
    if (existing) {
        int existingRSSI = [existing[@"rssi"] intValue];
        if (existingRSSI >= rssi) {
            return; // already have a stronger reading
        }
    }

    self.discovered[peripheralID] = @{
        @"uuid": peripheralID.UUIDString,
        @"name": name,
        @"rssi": @(rssi),
        @"is_lite": @(isLite ? 1 : 0),
    };
}

#pragma mark - Result building

- (WendyBLEScanResult)buildResult {
    NSArray<NSDictionary *> *values = [self.discovered allValues];
    int count = (int)values.count;
    if (count == 0) {
        return (WendyBLEScanResult){NULL, 0};
    }

    WendyBLEDevice *devices = (WendyBLEDevice *)calloc(count, sizeof(WendyBLEDevice));
    for (int i = 0; i < count; i++) {
        NSDictionary *d = values[i];
        devices[i].uuid = strdup([d[@"uuid"] UTF8String]);
        devices[i].name = strdup([d[@"name"] UTF8String]);
        devices[i].rssi = [d[@"rssi"] intValue];
        devices[i].is_lite = [d[@"is_lite"] intValue];
    }

    return (WendyBLEScanResult){devices, count};
}

@end

#pragma mark - C entry points

WendyBLEScanResult wendy_ble_scan(int scan_seconds) {
    @autoreleasepool {
        WendyBLEScanner *scanner = [[WendyBLEScanner alloc] init];

        // Wait for the Bluetooth adapter to become ready (up to 5 seconds).
        dispatch_semaphore_wait(scanner.readySem,
            dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC));

        if (!scanner.isReady) {
            return (WendyBLEScanResult){NULL, 0};
        }

        // Scan all peripherals. Wendy Lite (ESP32) devices do not advertise
        // their service UUID in BLE advertisement data, so we cannot rely on
        // CoreBluetooth's service-based filter. We filter by service UUID
        // and name prefix ("Wendy-") in the didDiscoverPeripheral callback.
        dispatch_async(scanner.bleQueue, ^{
            [scanner.centralManager scanForPeripheralsWithServices:nil
                                                           options:nil];
        });

        // Let the scan run for the requested duration. The dispatch queue
        // continues to process delegate callbacks while this thread sleeps.
        struct timespec ts = {scan_seconds, 0};
        nanosleep(&ts, NULL);

        // Stop scanning and drain any pending callbacks on the BLE queue.
        dispatch_sync(scanner.bleQueue, ^{
            [scanner.centralManager stopScan];
        });

        return [scanner buildResult];
    }
}

void wendy_ble_free_result(WendyBLEScanResult result) {
    if (result.devices == NULL) {
        return;
    }
    for (int i = 0; i < result.count; i++) {
        free((void *)result.devices[i].uuid);
        free((void *)result.devices[i].name);
    }
    free(result.devices);
}

#import <CoreBluetooth/CoreBluetooth.h>
#import <Foundation/Foundation.h>
#include <stdlib.h>
#include <string.h>
#include "scan_darwin.h"

// ── WendyBLEScanSession ─────────────────────────────────────────────
// A long-lived CoreBluetooth scan. Unlike the one-shot scanner in
// shared/discovery, this keeps scanning until stopped and lets Go sample the
// accumulated set whenever it likes, so there is no cgo->Go callback and
// nothing ever calls into Go from a CoreBluetooth thread.

@interface WendyBLEScanSession : NSObject <CBCentralManagerDelegate>

@property (nonatomic, strong) CBCentralManager *centralManager;
@property (nonatomic, strong) dispatch_queue_t bleQueue;
// discovered maps peripheral UUID string -> device dictionary. Guarded by
// discoveredLock: the delegate writes it on bleQueue while Go reads it from
// whatever goroutine calls snapshot.
@property (nonatomic, strong) NSMutableDictionary<NSString *, NSDictionary *> *discovered;
@property (nonatomic, strong) NSLock *discoveredLock;
@property (nonatomic) dispatch_semaphore_t readySem;
@property (nonatomic) BOOL isReady;
@property (nonatomic) BOOL scanning;

@end

@implementation WendyBLEScanSession

- (instancetype)init {
    self = [super init];
    if (self) {
        _discovered = [NSMutableDictionary new];
        _discoveredLock = [[NSLock alloc] init];
        _readySem = dispatch_semaphore_create(0);
        _isReady = NO;
        _scanning = NO;
        _bleQueue = dispatch_queue_create("sh.wendy.ble.scan.session", DISPATCH_QUEUE_SERIAL);
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
            // Bluetooth not usable — unblock the caller rather than hang.
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

    NSString *address = peripheral.identifier.UUIDString;
    if (address.length == 0) {
        return;
    }

    // The advertised local name is the live value; peripheral.name is
    // CoreBluetooth's cache (the GATT device name once the OS has connected)
    // and may be stale, so it is only a fallback.
    NSString *name = advertisementData[CBAdvertisementDataLocalNameKey];
    if (name.length == 0) {
        name = peripheral.name;
    }
    if (name == nil) {
        name = @"";
    }

    // CBUUID.UUIDString gives four hex characters for a 16-bit UUID and full
    // 128-bit form otherwise. Go's CanonicalUUID normalizes both.
    NSArray<CBUUID *> *serviceUUIDs = advertisementData[CBAdvertisementDataServiceUUIDsKey];
    NSMutableArray<NSString *> *uuidStrings = [NSMutableArray arrayWithCapacity:serviceUUIDs.count];
    for (CBUUID *svc in serviceUUIDs) {
        NSString *s = svc.UUIDString;
        if (s.length > 0) {
            [uuidStrings addObject:s];
        }
    }
    NSString *joined = [uuidStrings componentsJoinedByString:@","];

    [self.discoveredLock lock];
    NSDictionary *existing = self.discovered[address];
    // Preserve fields a later, sparser advertisement omits: a device may
    // advertise its name and its service UUIDs in different packets.
    if (existing) {
        if (name.length == 0) {
            name = existing[@"name"];
        }
        if (joined.length == 0) {
            joined = existing[@"service_uuids"];
        }
    }
    self.discovered[address] = @{
        @"name": name ?: @"",
        @"service_uuids": joined ?: @"",
        @"rssi": RSSI ?: @0,
    };
    [self.discoveredLock unlock];
}

#pragma mark - Session control

- (void)waitForReady:(int)timeoutSeconds {
    dispatch_semaphore_wait(self.readySem,
        dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeoutSeconds * NSEC_PER_SEC));
}

- (void)startScanning {
    // AllowDuplicates YES so RSSI and late-arriving advertisement fields keep
    // refreshing for the whole session; the one-shot scanner in
    // shared/discovery passes NO because it only needs each device once.
    // Services nil so nothing is filtered out here — see scan_darwin.h.
    dispatch_async(self.bleQueue, ^{
        [self.centralManager scanForPeripheralsWithServices:nil options:@{
            CBCentralManagerScanOptionAllowDuplicatesKey: @YES
        }];
    });
    self.scanning = YES;
}

- (void)stopScanning {
    if (!self.scanning) {
        return;
    }
    self.scanning = NO;
    // Synchronous so the delegate is quiet before the session is released.
    dispatch_sync(self.bleQueue, ^{
        [self.centralManager stopScan];
    });
}

- (WendyBLEScanSnapshot)buildSnapshot {
    [self.discoveredLock lock];
    NSArray<NSString *> *addresses = [self.discovered allKeys];
    int count = (int)addresses.count;
    if (count == 0) {
        [self.discoveredLock unlock];
        return (WendyBLEScanSnapshot){NULL, 0, NULL};
    }

    WendyBLEScanDeviceC *devices = (WendyBLEScanDeviceC *)calloc(count, sizeof(WendyBLEScanDeviceC));
    if (devices == NULL) {
        [self.discoveredLock unlock];
        return (WendyBLEScanSnapshot){NULL, 0, strdup("out of memory building BLE scan snapshot")};
    }
    for (int i = 0; i < count; i++) {
        NSString *address = addresses[i];
        NSDictionary *d = self.discovered[address];
        devices[i].address = strdup([address UTF8String]);
        devices[i].name = strdup([(d[@"name"] ?: @"") UTF8String]);
        devices[i].service_uuids = strdup([(d[@"service_uuids"] ?: @"") UTF8String]);
        devices[i].rssi = [d[@"rssi"] intValue];
    }
    [self.discoveredLock unlock];

    return (WendyBLEScanSnapshot){devices, count, NULL};
}

@end

#pragma mark - C entry points

int wendy_blescan_check(void) {
    @autoreleasepool {
        // A full session with a delegate, not just a manager: the abort from a
        // sandboxed terminal happens asynchronously when CoreBluetooth reaches
        // bluetoothd over XPC, so the state callback has to actually fire.
        WendyBLEScanSession *session = [[WendyBLEScanSession alloc] init];
        [session waitForReady:5];
        return session.isReady ? 0 : 1;
    }
}

WendyBLEScanHandle wendy_blescan_start(int ready_timeout_seconds) {
    @autoreleasepool {
        WendyBLEScanSession *session = [[WendyBLEScanSession alloc] init];
        [session waitForReady:ready_timeout_seconds];
        if (!session.isReady) {
            return NULL;
        }
        [session startScanning];
        // Hand ARC's reference to the caller; wendy_blescan_stop takes it back.
        return (WendyBLEScanHandle)CFBridgingRetain(session);
    }
}

WendyBLEScanSnapshot wendy_blescan_snapshot(WendyBLEScanHandle handle) {
    if (handle == NULL) {
        return (WendyBLEScanSnapshot){NULL, 0, strdup("BLE scan session is not running")};
    }
    @autoreleasepool {
        WendyBLEScanSession *session = (__bridge WendyBLEScanSession *)handle;
        return [session buildSnapshot];
    }
}

void wendy_blescan_free_snapshot(WendyBLEScanSnapshot snapshot) {
    if (snapshot.error != NULL) {
        free((void *)snapshot.error);
    }
    if (snapshot.devices == NULL) {
        return;
    }
    for (int i = 0; i < snapshot.count; i++) {
        free((void *)snapshot.devices[i].address);
        free((void *)snapshot.devices[i].name);
        free((void *)snapshot.devices[i].service_uuids);
    }
    free(snapshot.devices);
}

void wendy_blescan_stop(WendyBLEScanHandle handle) {
    if (handle == NULL) {
        return;
    }
    @autoreleasepool {
        WendyBLEScanSession *session = (WendyBLEScanSession *)CFBridgingRelease(handle);
        [session stopScanning];
    }
}

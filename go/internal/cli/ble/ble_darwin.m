#import <CoreBluetooth/CoreBluetooth.h>
#import <Foundation/Foundation.h>
#include "ble_darwin.h"

// ── WendyBLEConnection ──────────────────────────────────────────────
// Manages a single connection to a BLE peripheral including GATT and L2CAP.

@interface WendyBLEConnection : NSObject <CBCentralManagerDelegate, CBPeripheralDelegate>

@property (strong) CBCentralManager *centralManager;
@property (strong) CBPeripheral *peripheral;
@property (strong) dispatch_queue_t bleQueue;

// Connection state
@property (strong) dispatch_semaphore_t connectSema;
@property BOOL connected;
@property BOOL connectError;

// Service discovery
@property (strong) dispatch_semaphore_t discoverSema;
@property BOOL discoverDone;
@property BOOL discoverError;
@property int pendingCharDiscovery;

// Write state
@property (strong) dispatch_semaphore_t writeSema;
@property BOOL writeError;

// Read state
@property (strong) dispatch_semaphore_t readSema;
@property (strong) NSData *readData;
@property BOOL readError;

// Notification state
@property (strong) dispatch_semaphore_t notifySema;
@property (strong) NSMutableDictionary<NSString *, NSMutableArray<NSData *> *> *notifyQueues;
@property (strong) NSLock *notifyLock;

// L2CAP state
@property (strong) dispatch_semaphore_t l2capSema;
@property (strong) CBL2CAPChannel *l2capChannel;
@property BOOL l2capError;

// L2CAP receive state
@property (strong) NSMutableData *l2capRecvBuffer;
@property (strong) dispatch_semaphore_t l2capRecvSema;
@property (strong) NSLock *l2capRecvLock;

// L2CAP I/O thread — runs a real NSRunLoop so NSStreamDelegate events are delivered.
// Required in CLI binaries where the main run loop is never started.
@property (strong) NSThread *l2capIOThread;
@property BOOL l2capIORunning;
@property BOOL l2capOutputReady; // YES once outputStream fires NSStreamEventHasSpaceAvailable

// Write dispatch: wendy_ble_l2cap_send uses performSelector:onThread:waitUntilDone:YES
// so writes always happen on the I/O thread that owns the stream's run loop.
@property NSInteger l2capWriteResult;

// Target peripheral UUID for scanning
@property (strong) NSString *targetUUID;

@end

@implementation WendyBLEConnection

- (instancetype)init {
    self = [super init];
    if (self) {
        _bleQueue = dispatch_queue_create("sh.wendy.ble.client", DISPATCH_QUEUE_SERIAL);
        _connectSema = dispatch_semaphore_create(0);
        _discoverSema = dispatch_semaphore_create(0);
        _writeSema = dispatch_semaphore_create(0);
        _readSema = dispatch_semaphore_create(0);
        _notifySema = dispatch_semaphore_create(0);
        _l2capSema = dispatch_semaphore_create(0);
        _l2capRecvSema = dispatch_semaphore_create(0);
        _notifyQueues = [NSMutableDictionary dictionary];
        _notifyLock = [[NSLock alloc] init];
        _l2capRecvBuffer = [NSMutableData data];
        _l2capRecvLock = [[NSLock alloc] init];
    }
    return self;
}

// ── CBCentralManagerDelegate ────────────────────────────────────────

- (void)centralManagerDidUpdateState:(CBCentralManager *)central {
    if (central.state == CBManagerStatePoweredOn && self.targetUUID) {
        // Check the OS peripheral cache first. CoreBluetooth shares peripheral
        // knowledge across all CBCentralManager instances in the same process, so
        // the peripheral seen during discovery is already known here — no re-scan
        // needed. This avoids a 10-second timeout when the device stops advertising
        // between discovery and the connect call.
        NSUUID *uuid = [[NSUUID alloc] initWithUUIDString:self.targetUUID];
        if (uuid) {
            NSArray<CBPeripheral *> *known = [central retrievePeripheralsWithIdentifiers:@[uuid]];
            if (known.count > 0) {
                CBPeripheral *p = known[0];
                self.peripheral = p;
                p.delegate = self;
                [central connectPeripheral:p options:nil];
                return;
            }
        }
        // Not in cache — fall back to scanning.
        [central scanForPeripheralsWithServices:nil options:@{
            CBCentralManagerScanOptionAllowDuplicatesKey: @NO
        }];
    }
}

- (void)centralManager:(CBCentralManager *)central
 didDiscoverPeripheral:(CBPeripheral *)peripheral
     advertisementData:(NSDictionary<NSString *, id> *)advertisementData
                  RSSI:(NSNumber *)RSSI {
    if ([peripheral.identifier.UUIDString isEqualToString:self.targetUUID]) {
        [central stopScan];
        self.peripheral = peripheral;
        peripheral.delegate = self;
        [central connectPeripheral:peripheral options:nil];
    }
}

- (void)centralManager:(CBCentralManager *)central
  didConnectPeripheral:(CBPeripheral *)peripheral {
    self.connected = YES;
    dispatch_semaphore_signal(self.connectSema);
}

- (void)centralManager:(CBCentralManager *)central
didFailToConnectPeripheral:(CBPeripheral *)peripheral
                 error:(NSError *)error {
    self.connectError = YES;
    dispatch_semaphore_signal(self.connectSema);
}

- (void)centralManager:(CBCentralManager *)central
didDisconnectPeripheral:(CBPeripheral *)peripheral
                 error:(NSError *)error {
    self.connected = NO;
    self.l2capIORunning = NO; // wake the I/O thread so it exits
    // Signal any blocked operations
    dispatch_semaphore_signal(self.readSema);
    dispatch_semaphore_signal(self.writeSema);
    dispatch_semaphore_signal(self.l2capRecvSema);
}

// ── CBPeripheralDelegate ────────────────────────────────────────────

- (void)peripheral:(CBPeripheral *)peripheral
didDiscoverServices:(NSError *)error {
    if (error) {
        self.discoverError = YES;
        dispatch_semaphore_signal(self.discoverSema);
        return;
    }
    if (peripheral.services.count == 0) {
        self.discoverDone = YES;
        dispatch_semaphore_signal(self.discoverSema);
        return;
    }
    self.pendingCharDiscovery = (int)peripheral.services.count;
    for (CBService *svc in peripheral.services) {
        [peripheral discoverCharacteristics:nil forService:svc];
    }
}

- (void)peripheral:(CBPeripheral *)peripheral
didDiscoverCharacteristicsForService:(CBService *)service
             error:(NSError *)error {
    self.pendingCharDiscovery--;
    if (self.pendingCharDiscovery <= 0) {
        self.discoverDone = !error;
        self.discoverError = (error != nil);
        dispatch_semaphore_signal(self.discoverSema);
    }
}

- (void)peripheral:(CBPeripheral *)peripheral
didWriteValueForCharacteristic:(CBCharacteristic *)characteristic
             error:(NSError *)error {
    self.writeError = (error != nil);
    dispatch_semaphore_signal(self.writeSema);
}

- (void)peripheral:(CBPeripheral *)peripheral
didUpdateValueForCharacteristic:(CBCharacteristic *)characteristic
             error:(NSError *)error {
    if (error) {
        self.readError = YES;
        self.readData = nil;
        dispatch_semaphore_signal(self.readSema);
        return;
    }

    // Check if this is a notification for a subscribed characteristic
    NSString *key = [NSString stringWithFormat:@"%@:%@",
                     characteristic.service.UUID.UUIDString,
                     characteristic.UUID.UUIDString];

    [self.notifyLock lock];
    NSMutableArray *queue = self.notifyQueues[key];
    if (queue) {
        if (characteristic.value) {
            [queue addObject:[characteristic.value copy]];
        }
        [self.notifyLock unlock];
        dispatch_semaphore_signal(self.notifySema);
        return;
    }
    [self.notifyLock unlock];

    // Regular read response
    self.readData = characteristic.value ? [characteristic.value copy] : nil;
    self.readError = NO;
    dispatch_semaphore_signal(self.readSema);
}

- (void)peripheral:(CBPeripheral *)peripheral
didUpdateNotificationStateForCharacteristic:(CBCharacteristic *)characteristic
             error:(NSError *)error {
    // Handled by subscribe call checking isNotifying
    dispatch_semaphore_signal(self.writeSema); // reuse write sema for subscribe ack
}

- (void)peripheral:(CBPeripheral *)peripheral
didOpenL2CAPChannel:(CBL2CAPChannel *)channel
             error:(NSError *)error {
    if (error || !channel) {
        self.l2capError = YES;
        dispatch_semaphore_signal(self.l2capSema);
        return;
    }
    self.l2capChannel = channel;
    // Both streams are scheduled and opened on the I/O thread's run loop.
    // CBL2CAP output stream writes MUST come from the thread that owns the run
    // loop — writing from a foreign thread (e.g. the Go TLS goroutine) returns
    // -1 even when streamStatus is NSStreamStatusOpen.
    channel.inputStream.delegate = (id<NSStreamDelegate>)self;
    channel.outputStream.delegate = (id<NSStreamDelegate>)self;
    self.l2capIORunning = YES;
    self.l2capIOThread = [[NSThread alloc] initWithTarget:self
                                                 selector:@selector(l2capIOThreadMain)
                                                   object:nil];
    [self.l2capIOThread start];
}

// Runs on the dedicated L2CAP I/O thread.
// Schedules and opens BOTH streams so all NSStreamDelegate events are delivered
// here. Waits for NSStreamEventHasSpaceAvailable on the output stream before
// signalling l2capSema — that event confirms the BLE stack is ready for writes.
// All subsequent writes from wendy_ble_l2cap_send are dispatched here via
// performSelector:onThread:withObject:waitUntilDone:YES.
- (void)l2capIOThreadMain {
    @autoreleasepool {
        NSRunLoop *rl = [NSRunLoop currentRunLoop];

        [self.l2capChannel.inputStream scheduleInRunLoop:rl forMode:NSDefaultRunLoopMode];
        [self.l2capChannel.outputStream scheduleInRunLoop:rl forMode:NSDefaultRunLoopMode];
        [self.l2capChannel.inputStream open];
        [self.l2capChannel.outputStream open];

        // Wait until the output stream fires NSStreamEventHasSpaceAvailable,
        // which is the BLE stack's confirmation that writes are accepted.
        NSDate *readyDeadline = [NSDate dateWithTimeIntervalSinceNow:5.0];
        while (!self.l2capOutputReady && !self.l2capError) {
            if ([[NSDate date] compare:readyDeadline] != NSOrderedAscending) {
                self.l2capError = YES;
                break;
            }
            [rl runMode:NSDefaultRunLoopMode beforeDate:[NSDate dateWithTimeIntervalSinceNow:0.1]];
        }
        dispatch_semaphore_signal(self.l2capSema);


        while (self.l2capIORunning) {
            [rl runMode:NSDefaultRunLoopMode beforeDate:[NSDate dateWithTimeIntervalSinceNow:0.5]];
        }
        [self.l2capChannel.inputStream close];
        [self.l2capChannel.inputStream removeFromRunLoop:rl forMode:NSDefaultRunLoopMode];
        [self.l2capChannel.outputStream close];
        [self.l2capChannel.outputStream removeFromRunLoop:rl forMode:NSDefaultRunLoopMode];
    }
}

// Performs a single write on the I/O thread, called via performSelector:onThread:waitUntilDone:YES.
- (void)performL2CAPWrite:(NSData *)data {
    self.l2capWriteResult = [self.l2capChannel.outputStream write:data.bytes maxLength:data.length];
}

// ── NSStreamDelegate (both L2CAP streams) ───────────────────────────

- (void)stream:(NSStream *)aStream handleEvent:(NSStreamEvent)eventCode {
    if (aStream == self.l2capChannel.outputStream) {
        if (eventCode == NSStreamEventHasSpaceAvailable) {
            self.l2capOutputReady = YES;
        } else if (eventCode == NSStreamEventErrorOccurred || eventCode == NSStreamEventEndEncountered) {
            self.l2capError = YES;
            self.l2capIORunning = NO;
            dispatch_semaphore_signal(self.l2capRecvSema);
        }
        return;
    }

    if (eventCode == NSStreamEventHasBytesAvailable && aStream == self.l2capChannel.inputStream) {
        uint8_t buf[4096];
        NSInteger bytesRead = [(NSInputStream *)aStream read:buf maxLength:sizeof(buf)];
        if (bytesRead > 0) {
            [self.l2capRecvLock lock];
            [self.l2capRecvBuffer appendBytes:buf length:bytesRead];
            [self.l2capRecvLock unlock];
            dispatch_semaphore_signal(self.l2capRecvSema);
        }
    } else if (eventCode == NSStreamEventEndEncountered || eventCode == NSStreamEventErrorOccurred) {
        self.l2capIORunning = NO;
        dispatch_semaphore_signal(self.l2capRecvSema);
    }
}

// ── Helpers ─────────────────────────────────────────────────────────

- (CBCharacteristic *)findCharacteristic:(NSString *)charUUID inService:(NSString *)serviceUUID {
    CBUUID *svcUUID = [CBUUID UUIDWithString:serviceUUID];
    CBUUID *chrUUID = [CBUUID UUIDWithString:charUUID];
    for (CBService *svc in self.peripheral.services) {
        if ([svc.UUID isEqual:svcUUID]) {
            for (CBCharacteristic *chr in svc.characteristics) {
                if ([chr.UUID isEqual:chrUUID]) {
                    return chr;
                }
            }
        }
    }
    return nil;
}

@end

// ── C API Implementation ────────────────────────────────────────────

WendyBLEConn wendy_ble_connect(const char *peripheral_uuid, int timeout_seconds, WendyBLEError *out_error) {
    WendyBLEConnection *conn = [[WendyBLEConnection alloc] init];
    conn.targetUUID = [NSString stringWithUTF8String:peripheral_uuid];

    conn.centralManager = [[CBCentralManager alloc] initWithDelegate:conn
                                                               queue:conn.bleQueue
                                                             options:nil];

    long result = dispatch_semaphore_wait(conn.connectSema,
        dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeout_seconds * NSEC_PER_SEC));

    if (result != 0 || conn.connectError || !conn.connected) {
        if (out_error) *out_error = result != 0 ? WENDY_BLE_ERR_TIMEOUT : WENDY_BLE_ERR_CONNECT_FAILED;
        // Clean up
        if (conn.peripheral && conn.connected) {
            [conn.centralManager cancelPeripheralConnection:conn.peripheral];
        }
        return NULL;
    }

    if (out_error) *out_error = WENDY_BLE_OK;
    return (__bridge_retained void *)conn;
}

WendyBLEError wendy_ble_discover_services(WendyBLEConn handle, int timeout_seconds) {
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    if (!conn.connected) return WENDY_BLE_ERR_DISCONNECTED;

    conn.discoverDone = NO;
    conn.discoverError = NO;
    [conn.peripheral discoverServices:nil];

    long result = dispatch_semaphore_wait(conn.discoverSema,
        dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeout_seconds * NSEC_PER_SEC));

    if (result != 0) return WENDY_BLE_ERR_TIMEOUT;
    if (conn.discoverError) return WENDY_BLE_ERR_DISCOVER_FAILED;
    return WENDY_BLE_OK;
}

WendyBLEError wendy_ble_write_characteristic(WendyBLEConn handle, const char *service_uuid,
                                              const char *char_uuid, const uint8_t *data, int length) {
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    if (!conn.connected) return WENDY_BLE_ERR_DISCONNECTED;

    CBCharacteristic *chr = [conn findCharacteristic:[NSString stringWithUTF8String:char_uuid]
                                           inService:[NSString stringWithUTF8String:service_uuid]];
    if (!chr) return WENDY_BLE_ERR_NOT_FOUND;

    conn.writeError = NO;
    NSData *writeData = [NSData dataWithBytes:data length:length];
    [conn.peripheral writeValue:writeData forCharacteristic:chr type:CBCharacteristicWriteWithResponse];

    long result = dispatch_semaphore_wait(conn.writeSema,
        dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC));

    if (result != 0) return WENDY_BLE_ERR_TIMEOUT;
    if (conn.writeError) return WENDY_BLE_ERR_WRITE_FAILED;
    return WENDY_BLE_OK;
}

WendyBLEError wendy_ble_write_characteristic_no_response(WendyBLEConn handle, const char *service_uuid,
                                                          const char *char_uuid, const uint8_t *data, int length) {
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    if (!conn.connected) return WENDY_BLE_ERR_DISCONNECTED;

    CBCharacteristic *chr = [conn findCharacteristic:[NSString stringWithUTF8String:char_uuid]
                                           inService:[NSString stringWithUTF8String:service_uuid]];
    if (!chr) return WENDY_BLE_ERR_NOT_FOUND;

    NSData *writeData = [NSData dataWithBytes:data length:length];
    [conn.peripheral writeValue:writeData forCharacteristic:chr type:CBCharacteristicWriteWithoutResponse];
    return WENDY_BLE_OK;
}

WendyBLEReadResult wendy_ble_read_characteristic(WendyBLEConn handle, const char *service_uuid,
                                                  const char *char_uuid) {
    WendyBLEReadResult res = { .data = NULL, .length = 0, .error = WENDY_BLE_OK };
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    if (!conn.connected) { res.error = WENDY_BLE_ERR_DISCONNECTED; return res; }

    CBCharacteristic *chr = [conn findCharacteristic:[NSString stringWithUTF8String:char_uuid]
                                           inService:[NSString stringWithUTF8String:service_uuid]];
    if (!chr) { res.error = WENDY_BLE_ERR_NOT_FOUND; return res; }

    conn.readError = NO;
    conn.readData = nil;
    [conn.peripheral readValueForCharacteristic:chr];

    long result = dispatch_semaphore_wait(conn.readSema,
        dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC));

    if (result != 0) { res.error = WENDY_BLE_ERR_TIMEOUT; return res; }
    if (conn.readError) { res.error = WENDY_BLE_ERR_READ_FAILED; return res; }

    if (conn.readData && conn.readData.length > 0) {
        res.length = (int)conn.readData.length;
        res.data = (uint8_t *)malloc(res.length);
        memcpy(res.data, conn.readData.bytes, res.length);
    }
    return res;
}

WendyBLEError wendy_ble_subscribe(WendyBLEConn handle, const char *service_uuid,
                                   const char *char_uuid) {
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    if (!conn.connected) return WENDY_BLE_ERR_DISCONNECTED;

    CBCharacteristic *chr = [conn findCharacteristic:[NSString stringWithUTF8String:char_uuid]
                                           inService:[NSString stringWithUTF8String:service_uuid]];
    if (!chr) return WENDY_BLE_ERR_NOT_FOUND;

    // Set up notification queue
    NSString *key = [NSString stringWithFormat:@"%@:%@",
                     [NSString stringWithUTF8String:service_uuid],
                     [NSString stringWithUTF8String:char_uuid]];
    [conn.notifyLock lock];
    conn.notifyQueues[key] = [NSMutableArray array];
    [conn.notifyLock unlock];

    [conn.peripheral setNotifyValue:YES forCharacteristic:chr];

    long result = dispatch_semaphore_wait(conn.writeSema,
        dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC));

    if (result != 0) return WENDY_BLE_ERR_TIMEOUT;
    return WENDY_BLE_OK;
}

WendyBLEReadResult wendy_ble_wait_notification(WendyBLEConn handle, const char *service_uuid,
                                                const char *char_uuid, int timeout_seconds) {
    WendyBLEReadResult res = { .data = NULL, .length = 0, .error = WENDY_BLE_OK };
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    if (!conn.connected) { res.error = WENDY_BLE_ERR_DISCONNECTED; return res; }

    NSString *key = [NSString stringWithFormat:@"%@:%@",
                     [NSString stringWithUTF8String:service_uuid],
                     [NSString stringWithUTF8String:char_uuid]];

    // Check if there's already a queued notification
    [conn.notifyLock lock];
    NSMutableArray *queue = conn.notifyQueues[key];
    if (queue && queue.count > 0) {
        NSData *data = queue[0];
        [queue removeObjectAtIndex:0];
        [conn.notifyLock unlock];

        res.length = (int)data.length;
        res.data = (uint8_t *)malloc(res.length);
        memcpy(res.data, data.bytes, res.length);
        return res;
    }
    [conn.notifyLock unlock];

    // Wait for notification
    long result = dispatch_semaphore_wait(conn.notifySema,
        dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeout_seconds * NSEC_PER_SEC));

    if (result != 0) { res.error = WENDY_BLE_ERR_TIMEOUT; return res; }

    [conn.notifyLock lock];
    queue = conn.notifyQueues[key];
    if (queue && queue.count > 0) {
        NSData *data = queue[0];
        [queue removeObjectAtIndex:0];
        [conn.notifyLock unlock];

        res.length = (int)data.length;
        res.data = (uint8_t *)malloc(res.length);
        memcpy(res.data, data.bytes, res.length);
        return res;
    }
    [conn.notifyLock unlock];

    res.error = WENDY_BLE_ERR_TIMEOUT;
    return res;
}

WendyBLEError wendy_ble_open_l2cap(WendyBLEConn handle, uint16_t psm, int timeout_seconds) {
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    if (!conn.connected) return WENDY_BLE_ERR_DISCONNECTED;

    conn.l2capError = NO;
    conn.l2capChannel = nil;
    [conn.peripheral openL2CAPChannel:psm];

    long result = dispatch_semaphore_wait(conn.l2capSema,
        dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeout_seconds * NSEC_PER_SEC));

    if (result != 0) return WENDY_BLE_ERR_TIMEOUT;
    if (conn.l2capError || !conn.l2capChannel) return WENDY_BLE_ERR_L2CAP_FAILED;

    return WENDY_BLE_OK;
}

WendyBLEError wendy_ble_l2cap_send(WendyBLEConn handle, const uint8_t *data, int length) {
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    if (!conn.connected || !conn.l2capChannel || !conn.l2capIORunning) return WENDY_BLE_ERR_DISCONNECTED;

    // Dispatch the write to the I/O thread that owns the output stream's run loop.
    // CBL2CAP NSOutputStream writes must come from the owning run loop thread;
    // calling write:maxLength: from any other thread returns -1.
    NSData *writeData = [NSData dataWithBytes:data length:length];
    [conn performSelector:@selector(performL2CAPWrite:)
                 onThread:conn.l2capIOThread
               withObject:writeData
            waitUntilDone:YES];

    if (conn.l2capWriteResult < 0) return WENDY_BLE_ERR_WRITE_FAILED;
    return WENDY_BLE_OK;
}

WendyBLEL2CAPRecvResult wendy_ble_l2cap_recv(WendyBLEConn handle, int timeout_seconds) {
    WendyBLEL2CAPRecvResult res = { .data = NULL, .length = 0, .error = WENDY_BLE_OK };
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    if (!conn.connected || !conn.l2capChannel) { res.error = WENDY_BLE_ERR_DISCONNECTED; return res; }

    // Check if data is already buffered
    [conn.l2capRecvLock lock];
    if (conn.l2capRecvBuffer.length > 0) {
        res.length = (int)conn.l2capRecvBuffer.length;
        res.data = (uint8_t *)malloc(res.length);
        memcpy(res.data, conn.l2capRecvBuffer.bytes, res.length);
        conn.l2capRecvBuffer.length = 0;
        [conn.l2capRecvLock unlock];
        return res;
    }
    [conn.l2capRecvLock unlock];

    // Wait for data
    long result = dispatch_semaphore_wait(conn.l2capRecvSema,
        dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeout_seconds * NSEC_PER_SEC));

    if (result != 0) { res.error = WENDY_BLE_ERR_TIMEOUT; return res; }

    [conn.l2capRecvLock lock];
    if (conn.l2capRecvBuffer.length > 0) {
        res.length = (int)conn.l2capRecvBuffer.length;
        res.data = (uint8_t *)malloc(res.length);
        memcpy(res.data, conn.l2capRecvBuffer.bytes, res.length);
        conn.l2capRecvBuffer.length = 0;
    } else {
        res.error = WENDY_BLE_ERR_DISCONNECTED;
    }
    [conn.l2capRecvLock unlock];

    return res;
}

int wendy_ble_has_service(WendyBLEConn handle, const char *service_uuid) {
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    CBUUID *svcUUID = [CBUUID UUIDWithString:[NSString stringWithUTF8String:service_uuid]];
    for (CBService *svc in conn.peripheral.services) {
        if ([svc.UUID isEqual:svcUUID]) {
            return 1;
        }
    }
    return 0;
}

char *wendy_ble_list_services(WendyBLEConn handle) {
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    NSMutableArray<NSString *> *uuids = [NSMutableArray array];
    for (CBService *svc in conn.peripheral.services) {
        [uuids addObject:svc.UUID.UUIDString];
    }
    NSString *joined = [uuids componentsJoinedByString:@", "];
    return strdup([joined UTF8String]);
}

void wendy_ble_disconnect(WendyBLEConn handle) {
    if (!handle) return;
    WendyBLEConnection *conn = (__bridge_transfer WendyBLEConnection *)handle;

    conn.l2capIORunning = NO; // stop the I/O thread's run loop
    // Both streams are closed and unscheduled by the I/O thread when l2capIORunning goes NO.

    if (conn.peripheral && conn.connected) {
        [conn.centralManager cancelPeripheralConnection:conn.peripheral];
    }
}

void wendy_ble_free_data(uint8_t *data) {
    free(data);
}

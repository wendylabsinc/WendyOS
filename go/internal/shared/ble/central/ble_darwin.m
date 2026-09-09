#import <CoreBluetooth/CoreBluetooth.h>
#import <Foundation/Foundation.h>
#include "ble_darwin.h"

// Longest GATT characteristic value the Bluetooth core spec allows (Vol 3,
// Part F). A compliant peripheral cannot exceed this, so a longer value is
// treated as an error rather than truncated: a GATT value is one indivisible
// unit, and silently shortening it would corrupt the protocol above us.
static const NSUInteger kWendyBLEMaxGATTValue = 512;

// Most bytes one wendy_ble_l2cap_recv hands back. L2CAP is a byte stream, so
// returning less than is buffered is lossless — the remainder stays queued for
// the next call. Matches the linux path's per-read buffer (ble_linux.go), so
// both platforms cap a chunk at the same size.
static const NSUInteger kWendyBLEMaxL2CAPChunk = 65536;

// Copies n bytes into a fresh malloc'd buffer for the Go side to take over.
//
// Result lengths cross into Go as a 32-bit signed int while the sources are
// NSUInteger, so an unbounded cast could truncate or go negative and leave
// malloc, memcpy and C.GoBytes disagreeing about the size. Callers bound n
// first; this keeps all three agreeing on that one already-bounded value.
// Returns NO if the allocation fails, so no caller memcpys into NULL.
static BOOL wendyBLECopyOut(const void *bytes, NSUInteger n, uint8_t **outData, int *outLength) {
    // malloc(0) may legally return NULL, which would look like failure here.
    // An empty value is not an error: hand back NULL/0, which the Go side
    // already reads as "no data" and frees harmlessly.
    if (n == 0) {
        *outData = NULL;
        *outLength = 0;
        return YES;
    }
    uint8_t *copy = (uint8_t *)malloc(n);
    if (!copy) return NO;
    memcpy(copy, bytes, n);
    *outData = copy;
    *outLength = (int)n;
    return YES;
}

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

// Notification state. notifyCond guards notifyQueues and is broadcast on every
// enqueue and on disconnect. It replaces a lock plus a counting semaphore: the
// queues are per characteristic but one semaphore was shared by all of them, so
// a notification for one characteristic woke a waiter on another, and a value
// taken by the queued fast path left a count behind that made the next wait
// return instantly. A condition has no count to drift, and a waiter that wakes
// for someone else's characteristic simply loops and waits again.
@property (strong) NSCondition *notifyCond;
@property (strong) NSMutableDictionary<NSString *, NSMutableArray<NSData *> *> *notifyQueues;

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
        _l2capSema = dispatch_semaphore_create(0);
        _l2capRecvSema = dispatch_semaphore_create(0);
        _notifyQueues = [NSMutableDictionary dictionary];
        _notifyCond = [[NSCondition alloc] init];
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
    // Signal any blocked operations. A stray signal left on a semaphore nobody is
    // currently waiting on is harmless: every caller below checks conn.connected
    // before it would ever reach dispatch_semaphore_wait again.
    dispatch_semaphore_signal(self.readSema);
    dispatch_semaphore_signal(self.writeSema);
    dispatch_semaphore_signal(self.l2capRecvSema);
    dispatch_semaphore_signal(self.discoverSema);
    dispatch_semaphore_signal(self.l2capSema);
    // Broadcast rather than signal: every notification waiter has to learn the
    // link is gone, not just whichever one happens to be woken first.
    [self.notifyCond lock];
    [self.notifyCond broadcast];
    [self.notifyCond unlock];
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
    if (error) {
        self.discoverError = YES;
    }
    self.pendingCharDiscovery--;
    if (self.pendingCharDiscovery <= 0) {
        self.discoverDone = !self.discoverError;
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

    [self.notifyCond lock];
    NSMutableArray *queue = self.notifyQueues[key];
    if (queue) {
        if (characteristic.value) {
            [queue addObject:[characteristic.value copy]];
        }
        [self.notifyCond broadcast];
        [self.notifyCond unlock];
        return;
    }
    [self.notifyCond unlock];

    // Regular read response
    self.readData = characteristic.value ? [characteristic.value copy] : nil;
    self.readError = NO;
    dispatch_semaphore_signal(self.readSema);
}

- (void)peripheral:(CBPeripheral *)peripheral
didUpdateNotificationStateForCharacteristic:(CBCharacteristic *)characteristic
             error:(NSError *)error {
    self.writeError = (error != nil);
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
    if (!conn.connected) return WENDY_BLE_ERR_DISCONNECTED;
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
    if (!conn.connected) return WENDY_BLE_ERR_DISCONNECTED;
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
    if (!conn.connected) { res.error = WENDY_BLE_ERR_DISCONNECTED; return res; }
    if (conn.readError) { res.error = WENDY_BLE_ERR_READ_FAILED; return res; }

    if (conn.readData && conn.readData.length > 0) {
        if (conn.readData.length > kWendyBLEMaxGATTValue ||
            !wendyBLECopyOut(conn.readData.bytes, conn.readData.length, &res.data, &res.length)) {
            res.error = WENDY_BLE_ERR_READ_FAILED;
        }
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

    // Key the notification queue off CoreBluetooth's own CBUUID.UUIDString (via the
    // resolved characteristic) rather than the caller's spelling: for 16/32-bit UUIDs
    // CoreBluetooth's UUIDString is the short form ("180F"), which can differ from a
    // caller-supplied expanded 128-bit form. chr is the same CBCharacteristic instance
    // CoreBluetooth will hand back to didUpdateValueForCharacteristic:, so this
    // guarantees the keys match exactly.
    NSString *key = [NSString stringWithFormat:@"%@:%@",
                     chr.service.UUID.UUIDString, chr.UUID.UUIDString];
    [conn.notifyCond lock];
    conn.notifyQueues[key] = [NSMutableArray array];
    [conn.notifyCond unlock];

    conn.writeError = NO;
    [conn.peripheral setNotifyValue:YES forCharacteristic:chr];

    long result = dispatch_semaphore_wait(conn.writeSema,
        dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC));

    if (result != 0) return WENDY_BLE_ERR_TIMEOUT;
    if (!conn.connected) return WENDY_BLE_ERR_DISCONNECTED;
    if (conn.writeError) return WENDY_BLE_ERR_SUBSCRIBE_FAILED;
    return WENDY_BLE_OK;
}

WendyBLEReadResult wendy_ble_wait_notification(WendyBLEConn handle, const char *service_uuid,
                                                const char *char_uuid, int timeout_seconds) {
    WendyBLEReadResult res = { .data = NULL, .length = 0, .error = WENDY_BLE_OK };
    WendyBLEConnection *conn = (__bridge WendyBLEConnection *)handle;
    if (!conn.connected) { res.error = WENDY_BLE_ERR_DISCONNECTED; return res; }

    // Re-resolve the characteristic to derive the same canonical key wendy_ble_subscribe
    // used — see the comment there.
    CBCharacteristic *chr = [conn findCharacteristic:[NSString stringWithUTF8String:char_uuid]
                                           inService:[NSString stringWithUTF8String:service_uuid]];
    if (!chr) { res.error = WENDY_BLE_ERR_NOT_FOUND; return res; }

    NSString *key = [NSString stringWithFormat:@"%@:%@",
                     chr.service.UUID.UUIDString, chr.UUID.UUIDString];

    // One deadline for the whole call, fixed up front: a broadcast meant for
    // another characteristic sends us back to waiting, and recomputing the
    // timeout each time round would let those wakeups stretch the wait well
    // past what the caller asked for.
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:timeout_seconds];

    [conn.notifyCond lock];
    for (;;) {
        NSMutableArray *queue = conn.notifyQueues[key];
        if (queue && queue.count > 0) {
            NSData *data = queue[0];
            [queue removeObjectAtIndex:0];
            [conn.notifyCond unlock];

            if (data.length > kWendyBLEMaxGATTValue ||
                !wendyBLECopyOut(data.bytes, data.length, &res.data, &res.length)) {
                res.error = WENDY_BLE_ERR_READ_FAILED;
            }
            return res;
        }
        if (!conn.connected) {
            [conn.notifyCond unlock];
            res.error = WENDY_BLE_ERR_DISCONNECTED;
            return res;
        }
        if (![conn.notifyCond waitUntilDate:deadline]) {
            // NO means the deadline passed rather than a broadcast arriving.
            [conn.notifyCond unlock];
            res.error = conn.connected ? WENDY_BLE_ERR_TIMEOUT : WENDY_BLE_ERR_DISCONNECTED;
            return res;
        }
    }
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
    if (!conn.connected) return WENDY_BLE_ERR_DISCONNECTED;
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
        // Drain at most one chunk and keep the rest queued. L2CAP is a byte
        // stream, so a short return is lossless — l2capNetConn.Read stashes
        // whatever it can't take and the next call picks the remainder up here.
        NSUInteger n = MIN(conn.l2capRecvBuffer.length, kWendyBLEMaxL2CAPChunk);
        if (!wendyBLECopyOut(conn.l2capRecvBuffer.bytes, n, &res.data, &res.length)) {
            res.error = WENDY_BLE_ERR_L2CAP_FAILED;
        } else {
            [conn.l2capRecvBuffer replaceBytesInRange:NSMakeRange(0, n) withBytes:NULL length:0];
        }
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
        // Drain at most one chunk and keep the rest queued. L2CAP is a byte
        // stream, so a short return is lossless — l2capNetConn.Read stashes
        // whatever it can't take and the next call picks the remainder up here.
        NSUInteger n = MIN(conn.l2capRecvBuffer.length, kWendyBLEMaxL2CAPChunk);
        if (!wendyBLECopyOut(conn.l2capRecvBuffer.bytes, n, &res.data, &res.length)) {
            res.error = WENDY_BLE_ERR_L2CAP_FAILED;
        } else {
            [conn.l2capRecvBuffer replaceBytesInRange:NSMakeRange(0, n) withBytes:NULL length:0];
        }
    } else if (conn.connected && conn.l2capIORunning && !conn.l2capError) {
        // The buffered fast path above returns a whole chunk in one copy
        // without consuming a semaphore count, so several signalled arrivals
        // collapse into one return and leave the semaphore over-signalled. A
        // later wait then succeeds immediately with the buffer already empty.
        // The channel is still up, so report a timeout and let the caller retry
        // rather than tearing the link down.
        res.error = WENDY_BLE_ERR_TIMEOUT;
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

// Run the production native delegate and blocking C entry points without ever
// allocating CBCentralManager, opening a radio, or requesting TCC permission.
#define main notification_bounds_unused_main
#import "notification_bounds_test.m"
#undef main

@interface PendingPeripheral : NSObject
@property (strong) dispatch_semaphore_t entered;
@end
@implementation PendingPeripheral
- (void)discoverServices:(NSArray *)services { dispatch_semaphore_signal(self.entered); }
- (void)readValueForCharacteristic:(CBCharacteristic *)characteristic { dispatch_semaphore_signal(self.entered); }
- (void)writeValue:(NSData *)data forCharacteristic:(CBCharacteristic *)characteristic type:(CBCharacteristicWriteType)type { dispatch_semaphore_signal(self.entered); }
- (void)setNotifyValue:(BOOL)value forCharacteristic:(CBCharacteristic *)characteristic { dispatch_semaphore_signal(self.entered); }
- (void)openL2CAPChannel:(CBL2CAPPSM)psm { dispatch_semaphore_signal(self.entered); }
@end

// This observes entry but retains NSCondition's real lock/wait/broadcast behavior.
@interface ObservedCondition : NSCondition
@property (strong) dispatch_semaphore_t entered;
@property (strong) NSMutableSet<NSThread *> *participants;
@end
@implementation ObservedCondition
- (BOOL)waitUntilDate:(NSDate *)date {
    if (!self.participants) self.participants = [NSMutableSet set];
    if (![self.participants containsObject:NSThread.currentThread]) {
        [self.participants addObject:NSThread.currentThread];
        dispatch_semaphore_signal(self.entered);
    }
    return [super waitUntilDate:date];
}
@end

@interface ObservedReadLock : NSLock
@property (strong) dispatch_semaphore_t entered;
@end
@implementation ObservedReadLock
- (void)unlock { [super unlock]; dispatch_semaphore_signal(self.entered); }
@end

@interface ReceivedCharacteristic : TestCharacteristic
@property (strong) NSData *value;
@end
@implementation ReceivedCharacteristic
@end

static void requireCompletion(dispatch_semaphore_t done, const char *operation) {
    if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC)) != 0) {
        fprintf(stderr, "native disconnect failed to wake %s\n", operation);
        abort();
    }
}

static void pendingRequests(void) {
    const char *names[] = {"discovery", "read", "write", "subscribe", "l2cap-open", "l2cap-receive"};
    for (int kind = 0; kind < 6; kind++) {
        @autoreleasepool {
            NotificationConnection *conn = connection();
            PendingPeripheral *peripheral = [PendingPeripheral new];
            peripheral.entered = dispatch_semaphore_create(0);
            conn.peripheral = (CBPeripheral *)peripheral;
            if (kind == 5) {
                ObservedReadLock *lock = [ObservedReadLock new];
                lock.entered = peripheral.entered;
                conn.l2capRecvLock = lock;
                conn.l2capChannel = (CBL2CAPChannel *)[NSObject new];
                conn.l2capIORunning = YES;
            }
            dispatch_semaphore_t done = dispatch_semaphore_create(0);
            __block WendyBLEError result = WENDY_BLE_OK;
            dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
                WendyBLEConn handle = (__bridge WendyBLEConn)conn;
                switch (kind) {
                    case 0: result = wendy_ble_discover_services(handle, 5); break;
                    case 1: {
                        WendyBLEReadResult read = wendy_ble_read_characteristic(handle, "1800", "2A00");
                        result = read.error;
                        assert(read.data == NULL && read.length == 0);
                        wendy_ble_free_data(read.data);
                        break;
                    }
                    case 2: result = wendy_ble_write_characteristic(handle, "1800", "2A00", (const uint8_t *)"x", 1); break;
                    case 3: result = wendy_ble_subscribe(handle, "1800", "2A00"); break;
                    case 4: result = wendy_ble_open_l2cap(handle, 128, 5); break;
                    case 5: {
                        WendyBLEL2CAPRecvResult received = wendy_ble_l2cap_recv(handle, 5);
                        result = received.error;
                        assert(received.data == NULL && received.length == 0);
                        wendy_ble_free_data(received.data);
                        break;
                    }
                }
                dispatch_semaphore_signal(done);
            });
            requireCompletion(peripheral.entered, "request entry");
            assert(dispatch_semaphore_wait(done, DISPATCH_TIME_NOW) != 0);
            [conn centralManager:nil didDisconnectPeripheral:conn.peripheral error:nil];
            requireCompletion(done, names[kind]);
            assert(result == WENDY_BLE_ERR_DISCONNECTED);
            assert(!conn.connected && !conn.l2capIORunning);
            // Leftover semaphore counts cannot make another operation succeed.
            assert(wendy_ble_discover_services((__bridge WendyBLEConn)conn, 1) == WENDY_BLE_ERR_DISCONNECTED);
            assert(wendy_ble_read_characteristic((__bridge WendyBLEConn)conn, "1800", "2A00").error == WENDY_BLE_ERR_DISCONNECTED);
        }
    }
}

static void allNotificationWaiters(void) {
    NotificationConnection *conn = connection();
    ObservedCondition *condition = [ObservedCondition new];
    condition.entered = dispatch_semaphore_create(0);
    conn.notifyCond = condition;
    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    for (int i = 0; i < 3; i++) {
        dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
            const char *chr = i == 0 ? "2A00" : "2A01";
            WendyBLEReadResult result = wendy_ble_wait_notification((__bridge WendyBLEConn)conn, "1800", chr, 5);
            assert(result.error == WENDY_BLE_ERR_DISCONNECTED);
            assert(result.data == NULL && result.length == 0);
            dispatch_semaphore_signal(done);
        });
    }
    for (int i = 0; i < 3; i++) requireCompletion(condition.entered, "notification wait entry");
    // Acquiring this lock proves the last observed waiter released it into the
    // actual NSCondition wait, rather than only scheduling a future operation.
    [condition lock]; [condition unlock];
    assert(dispatch_semaphore_wait(done, DISPATCH_TIME_NOW) != 0);
    [conn centralManager:nil didDisconnectPeripheral:nil error:nil];
    for (int i = 0; i < 3; i++) requireCompletion(done, "all notification waiters");

    // Reconnection uses a new native connection, not stale queues or semaphores.
    NotificationConnection *fresh = connection();
    ReceivedCharacteristic *chr = [ReceivedCharacteristic new];
    chr.UUID = [CBUUID UUIDWithString:@"2A00"];
    chr.service = [TestService new]; chr.service.UUID = [CBUUID UUIDWithString:@"1800"];
    const uint8_t expected[] = {0, 0xff, 'n', 'e', 'w'};
    chr.value = [NSData dataWithBytes:expected length:sizeof(expected)];
    [fresh peripheral:nil didUpdateValueForCharacteristic:(CBCharacteristic *)chr error:nil];
    WendyBLEReadResult result = readNotification(fresh, "2A00");
    assert(result.error == WENDY_BLE_OK && result.length == sizeof(expected));
    assert(memcmp(result.data, expected, sizeof(expected)) == 0);
    wendy_ble_free_data(result.data);
}

int main(void) {
    @autoreleasepool {
        pendingRequests();
        allNotificationWaiters();
        puts("native disconnect waiters verified: six requests, three notifications, fresh delegate bytes");
    }
    return 0;
}

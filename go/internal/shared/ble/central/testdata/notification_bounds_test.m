#import "../ble_darwin.m"
#include <assert.h>

// Execute the real C entry points without starting a CoreBluetooth manager.
@interface TestService : NSObject
@property (strong) CBUUID *UUID;
@end
@implementation TestService
@end
@interface TestCharacteristic : NSObject
@property (strong) CBUUID *UUID;
@property (strong) TestService *service;
@end
@implementation TestCharacteristic
@end
@interface NotificationConnection : WendyBLEConnection
@property (strong) NSDictionary *characteristics;
@end
@implementation NotificationConnection
- (CBCharacteristic *)findCharacteristic:(NSString *)chr inService:(NSString *)service {
    return self.characteristics[chr];
}
@end

// A wakeup can belong to another characteristic or be spurious. Script that
// ordering while the real wait loop holds its lock; no timing sleeps are needed.
@interface ScriptedCondition : NSCondition
@property NSUInteger waits;
@property (strong) NSDate *firstDeadline;
@property (copy) void (^wake)(NSUInteger);
@end
@implementation ScriptedCondition
- (BOOL)waitUntilDate:(NSDate *)deadline {
    if (!self.firstDeadline) self.firstDeadline = deadline;
    assert([deadline isEqualToDate:self.firstDeadline]);
    self.waits++;
    assert(self.waits <= 3);
    self.wake(self.waits);
    return YES;
}
@end

// Do not allocate gigabytes to check that narrowing is rejected before copy.
@interface OversizedValue : NSObject
@end
@implementation OversizedValue
- (NSUInteger)length { return NSUIntegerMax; }
- (const void *)bytes { abort(); }
@end

static NotificationConnection *connection(void) {
    NotificationConnection *conn = [NotificationConnection new];
    conn.connected = YES;
    TestService *service = [TestService new];
    service.UUID = [CBUUID UUIDWithString:@"1800"];
    NSMutableDictionary *chars = [NSMutableDictionary dictionary];
    for (NSString *uuid in @[@"2A00", @"2A01"]) {
        TestCharacteristic *chr = [TestCharacteristic new];
        chr.UUID = [CBUUID UUIDWithString:uuid];
        chr.service = service;
        chars[uuid] = chr;
        conn.notifyQueues[[NSString stringWithFormat:@"1800:%@", uuid]] = [NSMutableArray array];
    }
    conn.characteristics = chars;
    return conn;
}

static WendyBLEReadResult readNotification(NotificationConnection *conn, const char *chr) {
    return wendy_ble_wait_notification((__bridge WendyBLEConn)conn, "1800", chr, 1);
}

static void notificationLengths(void) {
    for (NSUInteger n = 0; n <= 513; n += n == 0 ? 511 : 1) {
        NotificationConnection *conn = connection();
        NSData *value = [NSMutableData dataWithLength:n];
        [conn.notifyQueues[@"1800:2A00"] addObject:value];
        WendyBLEReadResult result = readNotification(conn, "2A00");
        if (n > 512) {
            assert(result.error == WENDY_BLE_ERR_READ_FAILED);
            assert(result.data == NULL && result.length == 0);
        } else {
            assert(result.error == WENDY_BLE_OK && result.length == (int)n);
            if (n) assert(memcmp(result.data, value.bytes, n) == 0);
        }
        wendy_ble_free_data(result.data);
    }
    NotificationConnection *conn = connection();
    [conn.notifyQueues[@"1800:2A00"] addObject:(NSData *)[OversizedValue new]];
    WendyBLEReadResult result = readNotification(conn, "2A00");
    assert(result.error == WENDY_BLE_ERR_READ_FAILED);
    assert(result.data == NULL && result.length == 0);
}

static void characteristicWakeups(void) {
    NotificationConnection *conn = connection();
    ScriptedCondition *condition = [ScriptedCondition new];
    conn.notifyCond = condition;
    NSData *a = [@"A" dataUsingEncoding:NSUTF8StringEncoding];
    NSData *b = [@"B" dataUsingEncoding:NSUTF8StringEncoding];
    condition.wake = ^(NSUInteger n) {
        [conn.notifyQueues[n == 1 ? @"1800:2A01" : @"1800:2A00"] addObject:n == 1 ? b : a];
    };
    WendyBLEReadResult result = readNotification(conn, "2A00");
    assert(condition.waits == 2 && result.error == WENDY_BLE_OK);
    assert(result.length == 1 && result.data[0] == 'A');
    wendy_ble_free_data(result.data);
    result = readNotification(conn, "2A01");
    assert(condition.waits == 2 && result.error == WENDY_BLE_OK);
    assert(result.length == 1 && result.data[0] == 'B');
    wendy_ble_free_data(result.data);
    // A queued fast-path delivery must not leave a stale wakeup for the next
    // wait. The next actual wake is a disconnect, which must retain its cause.
    condition.firstDeadline = nil;
    condition.wake = ^(NSUInteger n) { conn.connected = NO; };
    result = readNotification(conn, "2A01");
    assert(condition.waits == 3 && result.error == WENDY_BLE_ERR_DISCONNECTED);
    condition.wake = nil;
}

static void l2capRemainder(void) {
    WendyBLEConnection *conn = [WendyBLEConnection new];
    conn.connected = YES;
    conn.l2capChannel = (CBL2CAPChannel *)[NSObject new];
    conn.l2capRecvLock = [NSLock new];
    conn.l2capRecvBuffer = [NSMutableData dataWithLength:65536 + 19];
    memset(conn.l2capRecvBuffer.mutableBytes, 0xA1, 65536);
    memset((uint8_t *)conn.l2capRecvBuffer.mutableBytes + 65536, 0xB2, 19);
    WendyBLEL2CAPRecvResult first = wendy_ble_l2cap_recv((__bridge WendyBLEConn)conn, 0);
    assert(first.error == WENDY_BLE_OK && first.length == 65536);
    for (int i = 0; i < first.length; i++) assert(first.data[i] == 0xA1);
    assert(conn.l2capRecvBuffer.length == 19);
    WendyBLEL2CAPRecvResult last = wendy_ble_l2cap_recv((__bridge WendyBLEConn)conn, 0);
    assert(last.error == WENDY_BLE_OK && last.length == 19);
    for (int i = 0; i < last.length; i++) assert(last.data[i] == 0xB2);
    assert(conn.l2capRecvBuffer.length == 0);
    wendy_ble_free_data(first.data);
    wendy_ble_free_data(last.data);
}

int main(void) {
    @autoreleasepool {
        notificationLengths();
        characteristicWakeups();
        l2capRemainder();
    }
    return 0;
}

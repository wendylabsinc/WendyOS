#import "../ble_darwin.m"
#include <assert.h>

// Exercise the real native send path with an NSOutputStream that applies
// deterministic backpressure. No CoreBluetooth manager or radio is started.
@interface FakeOutput : NSOutputStream
@property (strong) NSMutableData *received;
@property (strong) NSArray<NSNumber *> *writes;
@property NSUInteger nextWrite;
@property BOOL ready;
@property BOOL resumeAfterZero;
@property BOOL disconnectAfterZero;
@property (weak) WendyBLEConnection *connection;
@end

@implementation FakeOutput
- (BOOL)hasSpaceAvailable { return self.ready; }
- (NSInteger)write:(const uint8_t *)bytes maxLength:(NSUInteger)length {
    assert([NSThread currentThread] == self.connection.l2capIOThread);
    NSInteger n = self.nextWrite < self.writes.count
        ? self.writes[self.nextWrite++].integerValue : (NSInteger)length;
    if (n > 0) {
        n = MIN(n, (NSInteger)length);
        [self.received appendBytes:bytes length:(NSUInteger)n];
    } else if (n == 0) {
        self.ready = NO;
        if (self.resumeAfterZero || self.disconnectAfterZero) {
            [NSTimer scheduledTimerWithTimeInterval:0.01 repeats:NO block:^(NSTimer *timer) {
                (void)timer;
                if (self.disconnectAfterZero)
                    self.connection.connected = NO;
                else
                    self.ready = YES;
            }];
        }
    }
    return n;
}
@end

@interface FakeChannel : NSObject
@property (strong) NSOutputStream *outputStream;
@end
@implementation FakeChannel
@end

@interface FakeConnection : WendyBLEConnection
@property (strong) dispatch_semaphore_t started;
@property (strong) dispatch_semaphore_t stopped;
@end
@implementation FakeConnection
- (void)l2capIOThreadMain {
    @autoreleasepool {
        NSRunLoop *loop = NSRunLoop.currentRunLoop;
        [loop addPort:[NSPort port] forMode:NSDefaultRunLoopMode];
        dispatch_semaphore_signal(self.started);
        while (self.l2capIORunning)
            [loop runMode:NSDefaultRunLoopMode beforeDate:[NSDate dateWithTimeIntervalSinceNow:0.01]];
        dispatch_semaphore_signal(self.stopped);
    }
}
@end

static FakeConnection *start(FakeOutput *output) {
    FakeConnection *conn = [FakeConnection new];
    conn.connected = YES;
    conn.l2capIORunning = YES;
    conn.started = dispatch_semaphore_create(0);
    conn.stopped = dispatch_semaphore_create(0);
    FakeChannel *channel = [FakeChannel new];
    channel.outputStream = output;
    conn.l2capChannel = (CBL2CAPChannel *)channel;
    output.connection = conn;
    output.received = [NSMutableData data];
    output.ready = YES;
    conn.l2capIOThread = [[NSThread alloc] initWithTarget:conn selector:@selector(l2capIOThreadMain) object:nil];
    [conn.l2capIOThread start];
    assert(dispatch_semaphore_wait(conn.started, dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC)) == 0);
    return conn;
}

static void stop(FakeConnection *conn) {
    conn.l2capIORunning = NO;
    assert(dispatch_semaphore_wait(conn.stopped, dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC)) == 0);
}

static void exercise(NSArray<NSNumber *> *writes, BOOL resume, BOOL disconnect,
                     WendyBLEError expected, NSString *received) {
    FakeOutput *output = [FakeOutput new];
    output.writes = writes;
    output.resumeAfterZero = resume;
    output.disconnectAfterZero = disconnect;
    FakeConnection *conn = start(output);
    WendyBLEConn handle = (__bridge WendyBLEConn)conn;
    const uint8_t data[] = "abcdefgh";
    NSTimeInterval before = NSProcessInfo.processInfo.systemUptime;
    WendyBLEError result = wendyBLEL2CAPSend(handle, data, sizeof(data) - 1, 0.2);
    assert(result == expected);
    assert(NSProcessInfo.processInfo.systemUptime - before < 1.0);
    assert([output.received isEqualToData:[received dataUsingEncoding:NSUTF8StringEncoding]]);
    if (expected != WENDY_BLE_OK) {
        assert(conn.l2capError);
        assert(!conn.l2capIORunning);
        assert(wendy_ble_l2cap_send(handle, data, sizeof(data) - 1) == WENDY_BLE_ERR_DISCONNECTED);
        assert([output.received isEqualToData:[received dataUsingEncoding:NSUTF8StringEncoding]]);
    }
    stop(conn);
}

int main(void) {
    @autoreleasepool {
        exercise(@[@2, @1, @3, @2], NO, NO, WENDY_BLE_OK, @"abcdefgh");
        exercise(@[@0, @2, @0, @6], YES, NO, WENDY_BLE_OK, @"abcdefgh");
        exercise(@[@3, @(-1)], NO, NO, WENDY_BLE_ERR_WRITE_FAILED, @"abc");
        exercise(@[@3, @0], NO, NO, WENDY_BLE_ERR_TIMEOUT, @"abc");
        exercise(@[@2, @0], NO, YES, WENDY_BLE_ERR_DISCONNECTED, @"ab");

        // A request queued onto an I/O thread that has already exited never
        // runs. The sender must notice the dead thread long before its deadline.
        FakeOutput *output = [FakeOutput new];
        FakeConnection *conn = start(output);
        stop(conn);
        conn.l2capIORunning = YES;
        NSTimeInterval before = NSProcessInfo.processInfo.systemUptime;
        assert(wendyBLEL2CAPSend((__bridge WendyBLEConn)conn, (const uint8_t *)"x", 1, 5.0)
               == WENDY_BLE_ERR_DISCONNECTED);
        assert(NSProcessInfo.processInfo.systemUptime - before < 1.0);
        assert(output.received.length == 0);
        assert(conn.l2capError);

        // Close aborts a send stalled under backpressure rather than waiting
        // out the write timeout.
        FakeOutput *stalled = [FakeOutput new];
        stalled.writes = @[@3, @0];
        FakeConnection *stalledConn = start(stalled);
        dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 50 * NSEC_PER_MSEC),
                       dispatch_get_global_queue(QOS_CLASS_DEFAULT, 0), ^{
            wendy_ble_l2cap_abort((__bridge WendyBLEConn)stalledConn);
        });
        before = NSProcessInfo.processInfo.systemUptime;
        assert(wendyBLEL2CAPSend((__bridge WendyBLEConn)stalledConn, (const uint8_t *)"abcdefgh", 8, 5.0)
               == WENDY_BLE_ERR_DISCONNECTED);
        assert(NSProcessInfo.processInfo.systemUptime - before < 1.0);
        assert([stalled.received isEqualToData:[@"abc" dataUsingEncoding:NSUTF8StringEncoding]]);
        stop(stalledConn);
    }
    return 0;
}

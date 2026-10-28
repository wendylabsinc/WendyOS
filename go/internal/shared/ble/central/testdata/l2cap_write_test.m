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
@property NSTimeInterval pauseAfterPositive;
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
        if (self.pauseAfterPositive > 0) {
            self.ready = NO;
            [NSTimer scheduledTimerWithTimeInterval:self.pauseAfterPositive repeats:NO block:^(NSTimer *timer) {
                (void)timer;
                self.ready = YES;
            }];
        }
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
    WendyBLEError result = wendyBLEL2CAPSend(handle, data, sizeof(data) - 1, 0.2, 1.0);
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

        // A certificate-sized ordinary request keeps exact bytes across the
        //16KiB boundary, transient backpressure and multiple partial writes.
        FakeOutput *certificateOutput = [FakeOutput new];
        certificateOutput.writes = @[@16384, @0, @3678, @0, @5584];
        certificateOutput.resumeAfterZero = YES;
        FakeConnection *certificateConn = start(certificateOutput);
        NSMutableData *certificate = [NSMutableData dataWithLength:25646];
        uint8_t *certificateBytes = certificate.mutableBytes;
        for (NSUInteger i = 0; i < certificate.length; i++) certificateBytes[i] = (uint8_t)(i * 17 + 3);
        assert(wendy_ble_l2cap_send((__bridge WendyBLEConn)certificateConn,
            certificate.bytes, certificate.length) == WENDY_BLE_OK);
        assert([certificateOutput.received isEqualToData:certificate]);
        stop(certificateConn);

        // Renewed progress is shared with the calling waiter, not just the
        // stream owner: total duration exceeds the idle budget several times.
        FakeOutput *paced = [FakeOutput new];
        paced.writes = @[@1, @1, @1, @1, @1, @1, @1, @1];
        paced.pauseAfterPositive = 0.04;
        FakeConnection *pacedConn = start(paced);
        NSTimeInterval pacedStart = NSProcessInfo.processInfo.systemUptime;
        assert(wendyBLEL2CAPSend((__bridge WendyBLEConn)pacedConn,
            (const uint8_t *)"abcdefgh", 8, 0.12, 1.5) == WENDY_BLE_OK);
        assert(NSProcessInfo.processInfo.systemUptime - pacedStart > 0.20);
        assert([paced.received isEqualToData:[@"abcdefgh" dataUsingEncoding:NSUTF8StringEncoding]]);
        stop(pacedConn);

        // Positive progress never extends the finite whole-write ceiling.
        FakeOutput *finite = [FakeOutput new];
        finite.writes = @[@1, @1, @1, @1, @1, @1, @1, @1];
        finite.pauseAfterPositive = 0.04;
        FakeConnection *finiteConn = start(finite);
        NSTimeInterval finiteStart = NSProcessInfo.processInfo.systemUptime;
        assert(wendyBLEL2CAPSend((__bridge WendyBLEConn)finiteConn,
            (const uint8_t *)"abcdefgh", 8, 0.12, 0.15) == WENDY_BLE_ERR_TIMEOUT);
        assert(NSProcessInfo.processInfo.systemUptime - finiteStart < 0.5);
        assert(finite.received.length > 0 && finite.received.length < 8);
        assert([finite.received isEqualToData:[[@"abcdefgh" dataUsingEncoding:NSUTF8StringEncoding]
            subdataWithRange:NSMakeRange(0, finite.received.length)]]);
        stop(finiteConn);

        // A caller can shorten an in-flight budget without waiting for the
        // stream owner's idle limit, or clear a previously expired deadline.
        FakeOutput *callerOutput = [FakeOutput new];
        callerOutput.writes = @[@1, @1, @1, @1, @1, @1, @1, @1];
        callerOutput.pauseAfterPositive = 0.04;
        FakeConnection *callerConn = start(callerOutput);
        dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 30 * NSEC_PER_MSEC),
                       dispatch_get_global_queue(QOS_CLASS_DEFAULT, 0), ^{
            wendy_ble_l2cap_set_write_deadline((__bridge WendyBLEConn)callerConn, 0.03, 1);
        });
        NSTimeInterval callerStart = NSProcessInfo.processInfo.systemUptime;
        assert(wendyBLEL2CAPSend((__bridge WendyBLEConn)callerConn,
            (const uint8_t *)"abcdefgh", 8, 0.3, 1.5) == WENDY_BLE_ERR_TIMEOUT);
        assert(NSProcessInfo.processInfo.systemUptime - callerStart < 0.5);
        assert(callerOutput.received.length > 0 && callerOutput.received.length < 8);
        stop(callerConn);

        FakeOutput *cleared = [FakeOutput new];
        FakeConnection *clearedConn = start(cleared);
        wendy_ble_l2cap_set_write_deadline((__bridge WendyBLEConn)clearedConn, -1, 1);
        wendy_ble_l2cap_set_write_deadline((__bridge WendyBLEConn)clearedConn, 0, 0);
        assert(wendyBLEL2CAPSend((__bridge WendyBLEConn)clearedConn,
            (const uint8_t *)"abcdefgh", 8, 0.1, 0.2) == WENDY_BLE_OK);
        assert([cleared.received isEqualToData:[@"abcdefgh" dataUsingEncoding:NSUTF8StringEncoding]]);
        stop(clearedConn);

        // A request queued onto an I/O thread that has already exited never
        // runs. The sender must notice the dead thread long before its deadline.
        FakeOutput *output = [FakeOutput new];
        FakeConnection *conn = start(output);
        stop(conn);
        conn.l2capIORunning = YES;
        NSTimeInterval before = NSProcessInfo.processInfo.systemUptime;
        assert(wendyBLEL2CAPSend((__bridge WendyBLEConn)conn, (const uint8_t *)"x", 1, 5.0, 5.0)
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
        assert(wendyBLEL2CAPSend((__bridge WendyBLEConn)stalledConn, (const uint8_t *)"abcdefgh", 8, 5.0, 5.0)
               == WENDY_BLE_ERR_DISCONNECTED);
        assert(NSProcessInfo.processInfo.systemUptime - before < 1.0);
        assert([stalled.received isEqualToData:[@"abc" dataUsingEncoding:NSUTF8StringEncoding]]);
        stop(stalledConn);
    }
    return 0;
}

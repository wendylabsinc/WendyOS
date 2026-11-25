// Exercise only the advertisement callback/snapshot bridge, never a radio.
#import <Foundation/Foundation.h>
#include <assert.h>
#include <string.h>
#include "../scan_darwin.m"

@interface FixturePeripheral : NSObject
@property(nonatomic,strong) NSUUID *identifier;
@property(nonatomic,strong) NSString *name;
@end
@implementation FixturePeripheral
@end

int main(void) {
 @autoreleasepool {
  // Bypass init: the production initializer starts a CoreBluetooth manager.
  WendyBLEScanSession *session=[WendyBLEScanSession alloc];
  session.discovered=[NSMutableDictionary new];session.discoveredLock=[NSLock new];
  FixturePeripheral *peer=[FixturePeripheral new];
  peer.identifier=[[NSUUID alloc]initWithUUIDString:@"11111111-1111-4111-8111-111111111111"];
  peer.name=@"cached display name";
  const uint8_t bytes[]={0x00,0xff,0x01,0x00};
  CBUUID *uuid=[CBUUID UUIDWithString:@"22222222-2222-4222-8222-222222222222"];
  [session centralManager:nil didDiscoverPeripheral:(CBPeripheral *)(id)peer
   advertisementData:@{CBAdvertisementDataServiceDataKey:@{uuid:[NSData dataWithBytes:bytes length:sizeof bytes]},CBAdvertisementDataLocalNameKey:@"live name"} RSSI:@-50];
  WendyBLEScanSnapshot first=[session buildSnapshot];
  assert(first.error==NULL && first.count==1);
  assert(first.devices[0].service_data_count==1);
  assert(first.devices[0].service_data[0].size==sizeof bytes);
  assert(memcmp(first.devices[0].service_data[0].bytes,bytes,sizeof bytes)==0);
  assert(strcmp(first.devices[0].advertisement_name,"live name")==0);
  double dataSeen=first.devices[0].service_data_seen;
  assert(dataSeen>0 && first.devices[0].last_seen>=dataSeen);
  wendy_blescan_free_snapshot(first);
  [session centralManager:nil didDiscoverPeripheral:(CBPeripheral *)(id)peer advertisementData:@{} RSSI:@-51];
  WendyBLEScanSnapshot sparse=[session buildSnapshot];
  assert(sparse.devices[0].service_data_seen==dataSeen);
  assert(sparse.devices[0].last_seen>=dataSeen);
  assert(memcmp(sparse.devices[0].service_data[0].bytes,bytes,sizeof bytes)==0);
  wendy_blescan_free_snapshot(sparse);
  for(unsigned i=0;i<1100;i++) {
   peer.identifier=[[NSUUID alloc]initWithUUIDString:[NSString stringWithFormat:@"%08x-3333-4333-8333-333333333333",i]];
   [session centralManager:nil didDiscoverPeripheral:(CBPeripheral *)(id)peer advertisementData:@{} RSSI:@-60];
  }
  assert(session.discovered.count==1024);
 }
 return 0;
}

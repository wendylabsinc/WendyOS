#include "../../../../include/WendyCore/WendyPlatform.h"
typedef struct FakeHost FakeHost;
FakeHost *fake_create(int mode);
void fake_free(FakeHost *host);
WendyHost fake_id(FakeHost *host);
uint64_t fake_stat(FakeHost *host, int field);
void fake_complete(WendyHostCall call, uint32_t code);
int fake_push(WendyHostCall call);
int fake_header(WendyHostCall call);

void fake_stale_complete(WendyHostCall call);
void fake_cloud_configure(FakeHost *host, WendyHostBytes key, WendyHostBytes first, WendyHostBytes second);
WendyHostBytes fake_cloud_view(FakeHost *host, int field);

typedef struct FakeCompletion FakeCompletion;
FakeCompletion *fake_cloud_create(WendyHost host, WendyDataView request);
void fake_cloud_start(FakeCompletion *completion);
void fake_cloud_cancel(FakeCompletion *completion);
void fake_cloud_release_operation(FakeCompletion *completion);
void fake_cloud_release_in_callback(FakeCompletion *completion);
void fake_cloud_free(FakeCompletion *completion);
uint64_t fake_cloud_stat(FakeCompletion *completion, int field);
WendyDataView fake_cloud_result(FakeCompletion *completion, int field);
int fake_cloud_invalid_create(void);

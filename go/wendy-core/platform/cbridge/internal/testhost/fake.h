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

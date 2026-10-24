#ifndef WENDY_BINDING_OBJECTS_H
#define WENDY_BINDING_OBJECTS_H
#include "callbacks.h"

WendyOperation *operation_new(uint64_t id);
uint64_t operation_id(WendyOperation const *operation);
void operation_complete(WendyCompletion completion, void *context, WendyData *data, WendyError *error);
WendyData *data_new(WendyDataView bytes);
WendyError *error_new(uint32_t code, WendyDataView message, WendyDataView details);
#endif

#ifndef WENDY_CHAT_TYPES_H
#define WENDY_CHAT_TYPES_H
typedef struct { char *id, *name; } WDChatModel;
typedef struct { WDStatus status; WDChatModel *items; size_t count; } WDChatModelsResult;
typedef enum { WD_CHAT_USER, WD_CHAT_ASSISTANT } WDChatRole;
typedef struct { WDChatRole role; char *text; } WDChatMessage;
typedef struct { char *kind, *title; uint32_t camera; } WDChatCard;
typedef struct { WDStatus status; WDChatCard *cards; size_t card_count; char *text, *error; int32_t done; char *activity, *trace; uint64_t approval_id; char *approval_action, *approval_app; } WDChatResult;
WDChatModelsResult *WDChatModels(uint64_t session, uint32_t port);
void WDChatModelsFree(WDChatModelsResult *value);
WDStatus *WDStartChat(uint64_t session, uint64_t turn, uint32_t port, char *model, WDChatMessage *messages, size_t count);
WDChatResult *WDPollChat(uint64_t session, uint64_t turn);
void WDChatFree(WDChatResult *value);
WDStatus *WDStopChat(uint64_t session, uint64_t turn);
WDStatus *WDResolveChatApproval(uint64_t session, uint64_t turn, uint64_t approval, int32_t allow);
#endif

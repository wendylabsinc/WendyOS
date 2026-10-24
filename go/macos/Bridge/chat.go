package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"unsafe"
)

//export WDChatModels
func WDChatModels(session C.uint64_t, port C.uint32_t) *C.WDChatModelsResult {
	p := alloc[C.WDChatModelsResult](1)
	items, err := desktop.ChatModels(uint64(session), uint32(port))
	p.status = status(err)
	p.items, p.count = alloc[C.WDChatModel](len(items)), C.size_t(len(items))
	for i, m := range items {
		unsafe.Slice(p.items, len(items))[i] = C.WDChatModel{id: C.CString(m.ID), name: C.CString(m.Name)}
	}
	return p
}

//export WDChatModelsFree
func WDChatModelsFree(p *C.WDChatModelsResult) {
	if p == nil {
		return
	}
	for _, m := range unsafe.Slice(p.items, int(p.count)) {
		free(m.id)
		free(m.name)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDStartChat
func WDStartChat(session, turn C.uint64_t, port C.uint32_t, model *C.char, messages *C.WDChatMessage, count C.size_t) *C.WDStatus {
	if count == 0 || count > 200 || messages == nil {
		return statusResult(errors.New("invalid conversation"))
	}
	var items []chat.Message
	for _, m := range unsafe.Slice(messages, int(count)) {
		role := "user"
		if m.role == C.WD_CHAT_ASSISTANT {
			role = "assistant"
		} else if m.role != C.WD_CHAT_USER {
			return statusResult(errors.New("invalid message role"))
		}
		items = append(items, chat.Message{Role: role, Content: C.GoString(m.text)})
	}
	return statusResult(desktop.StartChat(uint64(session), uint64(turn), uint32(port), C.GoString(model), items))
}

//export WDPollChat
func WDPollChat(session, turn C.uint64_t) *C.WDChatResult {
	p := alloc[C.WDChatResult](1)
	r, err := desktop.PollChat(uint64(session), uint64(turn))
	p.status = status(err)
	p.text, p.error, p.done = C.CString(r.Text), C.CString(r.Error), boolInt(r.Done)
	p.activity, p.trace = C.CString(r.Activity), C.CString(r.Trace)
	p.approval_id = C.uint64_t(r.ApprovalID)
	p.approval_action, p.approval_app = C.CString(r.ApprovalAction), C.CString(r.ApprovalApp)
	return p
}

//export WDChatFree
func WDChatFree(p *C.WDChatResult) {
	if p == nil {
		return
	}
	free(p.activity)
	free(p.trace)
	free(p.approval_action)
	free(p.approval_app)
	free(p.text)
	free(p.error)
	free(p.status.message)
	free(p)
}

//export WDStopChat
func WDStopChat(session, turn C.uint64_t) *C.WDStatus {
	return statusResult(desktop.StopChat(uint64(session), uint64(turn)))
}

//export WDResolveChatApproval
func WDResolveChatApproval(session, turn, approval C.uint64_t, allow C.int32_t) *C.WDStatus {
	return statusResult(desktop.ResolveChatApproval(uint64(session), uint64(turn), uint64(approval), allow != 0))
}

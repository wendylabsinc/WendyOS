// cmdns.c - Wraps mjansson/mdns static inline functions into callable symbols.

#include "mdns.h"
#include "include/cmdns.h"

// --- Socket operations ---

int cmdns_socket_open_ipv4(const struct sockaddr_in* saddr) {
    return mdns_socket_open_ipv4(saddr);
}

int cmdns_socket_open_ipv6(const struct sockaddr_in6* saddr) {
    return mdns_socket_open_ipv6(saddr);
}

void cmdns_socket_close(int sock) {
    mdns_socket_close(sock);
}

// --- Query send ---

int cmdns_query_send(int sock, uint16_t type,
                     const char* name, size_t name_length,
                     void* buffer, size_t capacity, uint16_t query_id) {
    return mdns_query_send(sock, (mdns_record_type_t)type, name, name_length,
                           buffer, capacity, query_id);
}

// --- Query recv with callback trampoline ---
// Our callback uses int for entry_type; mdns.h uses mdns_entry_type_t (enum).
// They're ABI-compatible (both int-sized), but we trampoline for type safety.

typedef struct {
    cmdns_callback_fn fn;
    void* user_data;
} trampoline_ctx_t;

static int trampoline_callback(
    int sock, const struct sockaddr* from, size_t addrlen,
    mdns_entry_type_t entry, uint16_t query_id, uint16_t rtype,
    uint16_t rclass, uint32_t ttl, const void* data, size_t size,
    size_t name_offset, size_t name_length, size_t record_offset,
    size_t record_length, void* user_data)
{
    trampoline_ctx_t* t = (trampoline_ctx_t*)user_data;
    return t->fn(sock, from, addrlen, (int)entry, query_id, rtype, rclass, ttl,
                 data, size, name_offset, name_length, record_offset, record_length,
                 t->user_data);
}

size_t cmdns_query_recv(int sock, void* buffer, size_t capacity,
                        cmdns_callback_fn callback, void* user_data,
                        int query_id) {
    trampoline_ctx_t t = { callback, user_data };
    return mdns_query_recv(sock, buffer, capacity, trampoline_callback, &t, query_id);
}

// --- Record parsing ---

cmdns_string_t cmdns_record_parse_ptr(const void* buffer, size_t size,
                                      size_t offset, size_t length,
                                      char* strbuffer, size_t capacity) {
    mdns_string_t r = mdns_record_parse_ptr(buffer, size, offset, length, strbuffer, capacity);
    cmdns_string_t ret = { r.str, r.length };
    return ret;
}

cmdns_srv_t cmdns_record_parse_srv(const void* buffer, size_t size,
                                   size_t offset, size_t length,
                                   char* strbuffer, size_t capacity) {
    mdns_record_srv_t r = mdns_record_parse_srv(buffer, size, offset, length, strbuffer, capacity);
    cmdns_srv_t ret;
    ret.priority = r.priority;
    ret.weight = r.weight;
    ret.port = r.port;
    ret.name.str = r.name.str;
    ret.name.length = r.name.length;
    return ret;
}

struct sockaddr_in* cmdns_record_parse_a(const void* buffer, size_t size,
                                         size_t offset, size_t length,
                                         struct sockaddr_in* addr) {
    return mdns_record_parse_a(buffer, size, offset, length, addr);
}

struct sockaddr_in6* cmdns_record_parse_aaaa(const void* buffer, size_t size,
                                             size_t offset, size_t length,
                                             struct sockaddr_in6* addr) {
    return mdns_record_parse_aaaa(buffer, size, offset, length, addr);
}

size_t cmdns_record_parse_txt(const void* buffer, size_t size,
                              size_t offset, size_t length,
                              cmdns_txt_t* records, size_t capacity) {
    // mdns_record_txt_t and cmdns_txt_t have identical layout
    return mdns_record_parse_txt(buffer, size, offset, length,
                                 (mdns_record_txt_t*)records, capacity);
}

cmdns_string_t cmdns_string_extract(const void* buffer, size_t size,
                                    size_t* offset,
                                    char* strbuffer, size_t capacity) {
    mdns_string_t r = mdns_string_extract(buffer, size, offset, strbuffer, capacity);
    cmdns_string_t ret = { r.str, r.length };
    return ret;
}

// cmdns.h - Thin wrapper around mjansson/mdns (public domain single-header mDNS library)
// Exposes non-inline functions callable from Swift.

#ifndef CMDNS_H
#define CMDNS_H

#include <stdint.h>
#include <stddef.h>
#include <sys/socket.h>
#include <netinet/in.h>

// Record types
#define CMDNS_RECORDTYPE_A     1
#define CMDNS_RECORDTYPE_PTR   12
#define CMDNS_RECORDTYPE_TXT   16
#define CMDNS_RECORDTYPE_AAAA  28
#define CMDNS_RECORDTYPE_SRV   33

// Entry types
#define CMDNS_ENTRYTYPE_QUESTION   0
#define CMDNS_ENTRYTYPE_ANSWER     1
#define CMDNS_ENTRYTYPE_AUTHORITY  2
#define CMDNS_ENTRYTYPE_ADDITIONAL 3

// Non-null-terminated string
typedef struct {
    const char* str;
    size_t length;
} cmdns_string_t;

// SRV record
typedef struct {
    uint16_t priority;
    uint16_t weight;
    uint16_t port;
    cmdns_string_t name;
} cmdns_srv_t;

// TXT key-value pair
typedef struct {
    cmdns_string_t key;
    cmdns_string_t value;
} cmdns_txt_t;

// Record callback. Return non-zero to stop parsing.
typedef int (*cmdns_callback_fn)(
    int sock,
    const struct sockaddr* from,
    size_t addrlen,
    int entry_type,
    uint16_t query_id,
    uint16_t rtype,
    uint16_t rclass,
    uint32_t ttl,
    const void* data,
    size_t size,
    size_t name_offset,
    size_t name_length,
    size_t record_offset,
    size_t record_length,
    void* user_data
);

// Socket operations
int cmdns_socket_open_ipv4(const struct sockaddr_in* saddr);
int cmdns_socket_open_ipv6(const struct sockaddr_in6* saddr);
void cmdns_socket_close(int sock);

// Send a PTR/SRV/etc query via multicast. Returns query_id or <0 on error.
int cmdns_query_send(int sock, uint16_t type,
                     const char* name, size_t name_length,
                     void* buffer, size_t capacity, uint16_t query_id);

// Receive and parse one response packet. Returns number of records parsed.
size_t cmdns_query_recv(int sock, void* buffer, size_t capacity,
                        cmdns_callback_fn callback, void* user_data,
                        int query_id);

// Record parsing
cmdns_string_t cmdns_record_parse_ptr(const void* buffer, size_t size,
                                      size_t offset, size_t length,
                                      char* strbuffer, size_t capacity);

cmdns_srv_t cmdns_record_parse_srv(const void* buffer, size_t size,
                                   size_t offset, size_t length,
                                   char* strbuffer, size_t capacity);

struct sockaddr_in* cmdns_record_parse_a(const void* buffer, size_t size,
                                         size_t offset, size_t length,
                                         struct sockaddr_in* addr);

struct sockaddr_in6* cmdns_record_parse_aaaa(const void* buffer, size_t size,
                                             size_t offset, size_t length,
                                             struct sockaddr_in6* addr);

size_t cmdns_record_parse_txt(const void* buffer, size_t size,
                              size_t offset, size_t length,
                              cmdns_txt_t* records, size_t capacity);

// Extract a DNS name at the given offset. Updates *offset past the name.
cmdns_string_t cmdns_string_extract(const void* buffer, size_t size,
                                    size_t* offset,
                                    char* strbuffer, size_t capacity);

// Wait for data on any of the given sockets using select().
// Returns number of ready sockets. ready_indices[] is filled with 0-based indices
// of sockets that have data available. timeout_ms is the max wait time.
int cmdns_select_multi(const int* sockets, int socket_count,
                       int* ready_indices, int timeout_ms);

// Open an IPv4 mDNS socket bound to a specific interface for both send and recv.
// Uses SO_BINDTODEVICE to force multicast through the correct interface.
// Falls back to IP_MULTICAST_IF if SO_BINDTODEVICE fails (needs CAP_NET_RAW).
int cmdns_socket_open_ipv4_iface(const struct sockaddr_in* saddr, const char* ifname);

#endif

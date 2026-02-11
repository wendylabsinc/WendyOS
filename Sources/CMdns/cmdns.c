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

#include <sys/select.h>

int cmdns_select_multi(const int* sockets, int socket_count,
                       int* ready_indices, int timeout_ms) {
    if (socket_count <= 0) return 0;

    fd_set readfds;
    FD_ZERO(&readfds);
    int maxfd = -1;
    for (int i = 0; i < socket_count; i++) {
        FD_SET(sockets[i], &readfds);
        if (sockets[i] > maxfd) maxfd = sockets[i];
    }

    struct timeval tv;
    tv.tv_sec = timeout_ms / 1000;
    tv.tv_usec = (timeout_ms % 1000) * 1000;

    int ret = select(maxfd + 1, &readfds, NULL, NULL, &tv);
    if (ret <= 0) return 0;

    int ready = 0;
    for (int i = 0; i < socket_count; i++) {
        if (FD_ISSET(sockets[i], &readfds)) {
            ready_indices[ready++] = i;
        }
    }
    return ready;
}

int cmdns_socket_open_ipv4_iface(const struct sockaddr_in* saddr, const char* ifname) {
    int sock = (int)socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (sock < 0) return -1;

    unsigned int reuseaddr = 1;
    setsockopt(sock, SOL_SOCKET, SO_REUSEADDR, (const char*)&reuseaddr, sizeof(reuseaddr));
#ifdef SO_REUSEPORT
    setsockopt(sock, SOL_SOCKET, SO_REUSEPORT, (const char*)&reuseaddr, sizeof(reuseaddr));
#endif

    unsigned char ttl = 1;
    unsigned char loopback = 1;
    setsockopt(sock, IPPROTO_IP, IP_MULTICAST_TTL, (const char*)&ttl, sizeof(ttl));
    setsockopt(sock, IPPROTO_IP, IP_MULTICAST_LOOP, (const char*)&loopback, sizeof(loopback));

    // Join multicast group on this interface
    struct ip_mreq req;
    memset(&req, 0, sizeof(req));
    req.imr_multiaddr.s_addr = htonl((((uint32_t)224U) << 24U) | ((uint32_t)251U));
    if (saddr)
        req.imr_interface = saddr->sin_addr;
    if (setsockopt(sock, IPPROTO_IP, IP_ADD_MEMBERSHIP, (char*)&req, sizeof(req))) {
        close(sock);
        return -1;
    }

    // Bind socket to a specific network device — ensures multicast goes out this interface.
    // Requires CAP_NET_RAW or root. Falls back to IP_MULTICAST_IF if unavailable.
    int bound_to_device = 0;
    if (ifname) {
#ifdef SO_BINDTODEVICE
        if (setsockopt(sock, SOL_SOCKET, SO_BINDTODEVICE, ifname, strlen(ifname)) == 0) {
            bound_to_device = 1;
        }
#endif
    }

    if (!bound_to_device && saddr) {
        // Fallback: try IP_MULTICAST_IF (may fail on musl)
        setsockopt(sock, IPPROTO_IP, IP_MULTICAST_IF, (const char*)&saddr->sin_addr,
                   sizeof(saddr->sin_addr));
    }

    // Bind to INADDR_ANY (needed to receive multicast)
    struct sockaddr_in bind_addr;
    if (saddr) {
        memcpy(&bind_addr, saddr, sizeof(bind_addr));
    } else {
        memset(&bind_addr, 0, sizeof(bind_addr));
        bind_addr.sin_family = AF_INET;
    }
    bind_addr.sin_addr.s_addr = INADDR_ANY;

    if (bind(sock, (struct sockaddr*)&bind_addr, sizeof(bind_addr))) {
        close(sock);
        return -1;
    }

    // Non-blocking
    const int flags = fcntl(sock, F_GETFL, 0);
    fcntl(sock, F_SETFL, flags | O_NONBLOCK);

    return sock;
}


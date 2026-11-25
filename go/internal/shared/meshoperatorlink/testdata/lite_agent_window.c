/* Compile against Wendy Lite's production control.c, binary.c and the common
 * binary writer. This fixture adds no firmware dependency to the Go build.
 * It checks the existing Lite parser, not a Go emulation of that parser. */
#include "wendy_net_control.h"
#include <assert.h>
#include <string.h>
int main(void) {
    unsigned char hello[64] = "\xa2\x61k\x75operator-router-hello\x61n\x50";
    for (unsigned i = 0; i < 16; i++) hello[28 + i] = i + 1;
    unsigned char instance[16];
    assert(!wn_operator_hello_read(hello, 44, instance));
    assert(!memcmp(instance, hello + 28, 16));
    hello[0] = 0xa3;
    const unsigned char window[] = {0x61, 'w', 0x1a, 0, 4, 0, 0};
    memcpy(hello + 44, window, sizeof window);
    assert(!wn_operator_hello_read(hello, 51, instance));
    assert(!memcmp(instance, hello + 28, 16));
    for (unsigned n = 0; n < 51; n++) assert(wn_operator_hello_read(hello, n, instance));
    unsigned char ready[64];
    assert(wn_operator_ready_write(ready, sizeof ready) == 25);
    assert(!memcmp(ready, "\xa1\x61k\x75operator-router-ready", 25));
    return 0;
}

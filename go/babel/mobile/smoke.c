// C ABI smoke test; built manually against the generated archive and header.
//go:build ignore
#include "babel.h"
#include <assert.h>
#include <string.h>
int main(void) {
    uintptr_t h = BabelNew("1", 0);
    assert(h != 0);
    char *r = BabelStep(h, 0, "{\"Type\":\"originate\",\"Prefix\":\"fd00::1/128\"}");
    assert(r && strstr(r, "\"Revision\":1"));
    BabelFree(r);
    r = BabelCommit(h, 1, 1);
    assert(r && !strstr(r, "Error"));
    BabelFree(r);
    char *state = BabelCheckpoint(h);
    uintptr_t restored = BabelRestore(state);
    assert(restored != 0);
    BabelFree(state);
    BabelDestroy(restored);
    BabelDestroy(h);
    return 0;
}

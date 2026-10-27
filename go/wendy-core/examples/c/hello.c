#include <WendyCore/WendyCore.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

int main(void) {
    int32_t const result = WendyHelloWorld();
    printf("WendyHelloWorld() = %d\n", result);
    char const *version = WendyCoreVersion();
    char const *expected = getenv("WENDY_CORE_VERSION");
    printf("WendyCoreVersion() = %s\n", version);
    if (version == NULL || version[0] == '\0' || version != WendyCoreVersion()) return 1;
    if (expected != NULL && strcmp(version, expected) != 0) return 1;
    return result == 42 ? 0 : 1;
}

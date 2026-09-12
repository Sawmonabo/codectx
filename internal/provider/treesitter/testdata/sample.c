#include <stdio.h>
#include "sample.h"

/* Non-ASCII: héllo → 日本 */
static const char *greeting = "héllo → 日本";

#define PORT 8080
#define SQUARE(x) ((x) * (x))

/* A server. */
struct server {
    const char *name;
    int port;
};

typedef struct server server_t;

enum mode { FAST, SLOW };

static int helper(const char *name);

/* Start the server. */
int start(server_t *s) {
    struct local { int x; } l = {SQUARE(2)};
    printf("%s %d\n", greeting, l.x);
    return helper(s->name);
}

static int helper(const char *name) { return name[0]; }

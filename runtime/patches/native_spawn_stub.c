#include "kernel/native_spawn.h"
#include "kernel/errno.h"
#include <spawn.h>
#include <stdbool.h>
#include <stdlib.h>

/* Placeholder. native_spawn_linux.c provides the Linux implementation; this
 * keeps the API linkable on platforms that have no spawn executor yet. */
#if !defined(__linux__)
int native_spawn_supported(void) { return 0; }
int native_spawn_run(const struct native_spawn_request *req, pid_t *pid_out) {
    (void)req; (void)pid_out; return _ENOSYS;
}
int native_spawn_wait(pid_t pid, int *status_out) {
    (void)pid; (void)status_out; return _ENOSYS;
}
int native_spawn_kill(pid_t pid, int sig) {
    (void)pid; (void)sig; return _ENOSYS;
}
#endif

#ifndef NATIVE_SPAWN_H
#define NATIVE_SPAWN_H

#include <stdbool.h>
#include <stddef.h>
#include <sys/types.h>

// ============================================================================
// Native process spawn (Linux host)
// ============================================================================
//
// Starts a host process that stands in for a guest exec. This is the Linux
// counterpart of the macOS posix_spawn path: the guest process model (pid,
// SIGCHLD, wait4 status) is expected to be provided by the caller, which
// creates a placeholder task and reaps this pid when it exits.
//
// stdio descriptors are host fds owned by the caller. A negative value means
// "connect to /dev/null" for stdin and "leave the parent's fd" for stdout and
// stderr.

struct native_spawn_request {
    const char *host_path;      // absolute host path of the executable
    const char *argv;           // NUL-separated guest argv, argc entries
    size_t argc;
    const char *envp;           // NUL-separated guest envp
    bool reuse_host_env;        // ignore envp and inherit the host environment
    int stdin_fd;
    int stdout_fd;
    int stderr_fd;
    const char *host_argv;      // optional: replaces argv[0] after stdio setup
    const char *host_envp;      // optional: NULL-separated host environment
    const struct native_spawn_stdio *stdio;  // continue_in_child fd wiring
    const char *cwd;            // host working directory, NULL keeps parent's
    bool handle_console;        // ignore SIGTTIN/SIGTTOU in the child
    // When true, a successful call returns NATIVE_SPAWN_CHILD in the child and
    // the caller continues there with stdio redirected. Only permitted after
    // every other host thread has been stopped by the caller.
    bool continue_in_child;
};

#define NATIVE_SPAWN_CHILD 1

// Child-side steps, safe to call between fork() and the guest continuation.
int ish_spawn_select_interp(const char *path, char *out, size_t out_size);
int ish_spawn_stat_mode(const char *path, unsigned *mode);
void ish_spawn_become_single_threaded(void);
int ish_spawn_exec_guest(const char *path, char **argv, char **envp);

// Child-side stdio wiring, applied only for continue_in_child requests.
struct native_spawn_stdio {
    int host_fd[3];  // host fds to dup onto 0/1/2, -1 leaves the slot alone
};

int native_spawn_supported(void);
// Map a guest path to the host path a child would need. Returns 1 and fills
// host_out, 0 when the path can only be resolved through the guest VFS, and -1
// on error.
int native_spawn_host_path(const char *guest_path, char *host_out, size_t host_size);
int native_spawn_run(const struct native_spawn_request *req, pid_t *pid_out);
int native_spawn_wait(pid_t pid, int *status_out);
int native_spawn_kill(pid_t pid, int sig);

#endif

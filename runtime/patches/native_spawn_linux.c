/* Native spawn executor for the Linux/iSH host.
 *
 * On macOS, native offload uses posix_spawn. Here we use plain fork()+exec on
 * the guest thread. The child exists only between fork and exec (or _exit), so
 * it never runs a guest program natively while the emulator is also running it:
 * the risk this file guards against is the child calling a non-async-signal-safe
 * libc function before exec. Only stdio pipe plumbing and execve are used there.
 *
 * Filesystem access is redirected through the host filesystem, not the fakefs
 * VFS, so the guest has to hand over translated host paths. That translation is
 * the caller's job: we only honour absolute guest paths whose host mapping we
 * are told about.
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <spawn.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#include "kernel/errno.h"
#include <limits.h>
#include "kernel/fs.h"
#include "platform/platform.h"
#include "kernel/native_spawn.h"

extern char **environ;

/* Build argv for the child: argv[0] is replaced by the host path. */
static char **build_argv(const char *host_path, size_t argc, const char *argv) {
    char **out = calloc(argc + 1, sizeof(char *));
    if (out == NULL)
        return NULL;
    out[0] = strdup(host_path);
    const char *p = argv;
    for (size_t i = 0; i < argc; i++) {
        if (i > 0)
            out[i] = strdup(p);
        p += strlen(p) + 1;
    }
    out[argc] = NULL;
    return out;
}

/* Copy the guest environment blob into a NULL-terminated vector. */
static char **build_env(const char *envp) {
    if (envp == NULL)
        return NULL;
    size_t n = 0;
    for (const char *p = envp; *p != '\0'; p += strlen(p) + 1)
        n++;
    char **out = calloc(n + 1, sizeof(char *));
    if (out == NULL)
        return NULL;
    size_t i = 0;
    for (const char *p = envp; *p != '\0'; p += strlen(p) + 1)
        out[i++] = strdup(p);
    out[n] = NULL;
    return out;
}

static void free_vec(char **v) {
    if (v == NULL)
        return;
    for (char **p = v; *p != NULL; p++)
        free(*p);
    free(v);
}

extern struct mount *g_fakefs_mount;
extern bool fakefs_bind_mount_translate_path(const char *path, char *out_path,
        size_t out_size);

int native_spawn_host_path(const char *guest_path, char *host_out, size_t host_size) {
    if (guest_path == NULL || host_out == NULL || host_size == 0)
        return -1;
    if (guest_path[0] != '/')
        return 0;
    if (fakefs_bind_mount_translate_path(guest_path, host_out, host_size))
        return 1;
    if (g_fakefs_mount == NULL || g_fakefs_mount->root_fd < 0)
        return 0;
    /* Fakefs stores every path relative to its data directory; resolve that
     * directory through the mount fd so the guest never needs to know it. */
    char root[PATH_MAX];
    if (platform_fd_get_path(g_fakefs_mount->root_fd, root, sizeof(root)) < 0)
        return 0;
    int n = snprintf(host_out, host_size, "%s/%s", root, guest_path + 1);
    if (n < 0 || (size_t)n >= host_size)
        return -1;
    return 1;
}

int native_spawn_supported(void) {
    return 1;
}

int native_spawn_run(const struct native_spawn_request *req, pid_t *pid_out) {
    if (req == NULL || req->host_path == NULL || req->argv == NULL ||
            req->argc == 0 || req->stdin_fd < -1 ||
            req->stdout_fd < -1 || req->stderr_fd < -1)
        return _EINVAL;

    char **argv = build_argv(req->host_path, req->argc, req->argv);
    if (argv == NULL)
        return _ENOMEM;
    char **envp = req->reuse_host_env ? environ : build_env(req->envp);
    if (envp == NULL && !req->reuse_host_env) {
        free_vec(argv);
        return _ENOMEM;
    }

    /* Every descriptor the child must inherit has to be non-CLOEXEC in the
     * parent for the duration of the fork. Save the flags and restore them. */
    int flags_before[3] = { -1, -1, -1 };
    const int fds[3] = { req->stdin_fd, req->stdout_fd, req->stderr_fd };
    for (int i = 0; i < 3; i++) {
        if (fds[i] >= 0) {
            flags_before[i] = fcntl(fds[i], F_GETFD);
            if (flags_before[i] >= 0)
                fcntl(fds[i], F_SETFD, flags_before[i] & ~FD_CLOEXEC);
        }
    }

    pid_t pid = fork();
    if (pid == 0) {
        /* Child: async-signal-safe calls only. A relative host_path would be
         * resolved after chdir, so it is only accepted without a chdir.
         * If the request carries a continuation, the child hands control back
         * to its caller after stdio is in place and never execs: the caller
         * runs in a dedicated single-threaded copy of the emulator. */
        if (fds[0] >= 0 && fds[0] != 0) {
            if (dup2(fds[0], 0) < 0) _exit(127);
        } else if (fds[0] < 0) {
            int devnull = open("/dev/null", O_RDONLY);
            if (devnull >= 0) {
                dup2(devnull, 0);
                if (devnull != 0) close(devnull);
            }
        }
        if (fds[1] >= 0 && fds[1] != 1) {
            if (dup2(fds[1], 1) < 0) _exit(127);
        }
        if (fds[2] >= 0 && fds[2] != 2) {
            if (dup2(fds[2], 2) < 0) _exit(127);
        }
        if (req->cwd != NULL && req->host_path[0] != '/')
            _exit(127);
        if (req->cwd != NULL)
            chdir(req->cwd);
        /* Restore the guest signal dispositions the child would inherit
         * changing either, so reset the ones that would silence it. */
        signal(SIGPIPE, SIG_DFL);
        signal(SIGINT, SIG_DFL);
        signal(SIGQUIT, SIG_DFL);
        if (req->host_argv != NULL) {
            /* The guest argv may contain virtual paths; the caller supplies the
             * exact strings the host process must see. */
            free_vec(argv);
            argv = build_argv(req->host_argv, req->argc, req->argv);
            if (argv == NULL)
                _exit(127);
            argv[0] = strdup(req->host_argv);
        }
        if (req->host_envp != NULL) {
            free_vec(envp);
            envp = build_env(req->host_envp);
            if (envp == NULL)
                _exit(127);
        }
        if (req->handle_console) {
            /* The emulator owns SIGTTIN/SIGTTOU handling; a child started from
             * a background job must not be stopped by them. */
            signal(SIGTTOU, SIG_IGN);
            signal(SIGTTIN, SIG_IGN);
        }
        if (req->continue_in_child) {
            if (req->stdio != NULL) {
                for (int i = 0; i < 3; i++) {
                    int fd = req->stdio->host_fd[i];
                    if (fd < 0)
                        continue;
                    if (fd != i) {
                        if (dup2(fd, i) < 0)
                            _exit(127);
                        close(fd);
                    }
                }
            }
            return NATIVE_SPAWN_CHILD;
        }
        execve(req->host_path, argv, envp);
        _exit(127);
    }

    int err = 0;
    if (pid < 0) {
        err = errno;
    }

    for (int i = 0; i < 3; i++) {
        if (fds[i] >= 0 && flags_before[i] >= 0)
            fcntl(fds[i], F_SETFD, flags_before[i]);
    }
    free_vec(argv);
    if (!req->reuse_host_env)
        free_vec(envp);

    if (err != 0)
        return err;
    if (pid != 0 && pid_out != NULL)
        *pid_out = pid;
    return 0;
}

int native_spawn_wait(pid_t pid, int *status_out) {
    int status = 0;
    pid_t r;
    do {
        r = waitpid(pid, &status, 0);
    } while (r < 0 && errno == EINTR);
    if (r < 0)
        return errno;
    if (status_out != NULL)
        *status_out = status;
    return 0;
}

int native_spawn_kill(pid_t pid, int sig) {
    if (kill(pid, sig) < 0)
        return errno;
    return 0;
}

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <errno.h>
#include <signal.h>
#include <sys/stat.h>
#include <sys/syscall.h>

/* Child-side helper for native spawn. Runs between fork() and either execve()
 * of the guest program or handing control back to the guest kernel.
 * Everything here must be async-signal-safe. */

int ish_spawn_select_interp(const char *path, char *out, size_t out_size) {
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0)
        return -1;
    char buf[256];
    ssize_t n = read(fd, buf, sizeof(buf) - 1);
    close(fd);
    if (n <= 2 || buf[0] != '#' || buf[1] != '!')
        return 0;
    buf[n] = '\0';
    if (buf[2] == '/') {
        if ((size_t)n >= out_size)
            return -1;
        memcpy(out, buf + 2, (size_t)n - 1);
        return 1;
    }
    return 0;
}

int ish_spawn_stat_mode(const char *path, unsigned *mode) {
    struct stat st;
    if (stat(path, &st) < 0)
        return -1;
    *mode = (unsigned)st.st_mode;
    return 0;
}

/* Single-threaded guest continuation: keep only the guest thread. */
void ish_spawn_become_single_threaded(void) {
    signal(SIGPIPE, SIG_DFL);
    signal(SIGCHLD, SIG_DFL);
    signal(SIGINT, SIG_DFL);
    sigset_t empty;
    sigemptyset(&empty);
    sigprocmask(SIG_SETMASK, &empty, NULL);
    /* Helpers may have blocked these; they are per-thread masks. */
    signal(SIGTERM, SIG_DFL);
}

/* Execve the guest binary. Because the child owns the process image, execve
 * replaces it in place. Returns only on failure (errno set). */
int ish_spawn_exec_guest(const char *path, char **argv, char **envp) {
    execve(path, argv, envp);
    return -1;
}

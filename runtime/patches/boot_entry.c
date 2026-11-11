#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#include <limits.h>

#include "kernel/calls.h"
#include "kernel/errno.h"
#include "kernel/fs.h"
#include "kernel/task.h"
#include "xX_main_Xx.h"

/* Guest entry for one-shot runs.
 *
 * ISH_BOOT_ARGS: the runtime host restarts this executable per run, so the
 * guest gets the process to itself and its command line arrives in the
 * environment (PB_ARGV_B64, a JSON array) rather than on the host argv, which
 * the runtime host cannot pass reliably. ISH_ROOTFS names the fakefs. The call
 * never returns: xX_main_Xx runs the guest task on this thread.
 *
 * Without ISH_BOOT_ARGS this is a sh entry point it consumes with the CLI. */

extern int b64_decode(const char *in, unsigned char *out, size_t out_size);

/* ISH_BOOT_PIPE: the host keeps the guest in a persistent shell. Commands
 * arrive on this fd as NUL-terminated lines, and the shell is started with
 * ISH_SHELL_SCRIPT pointing at a FIFO-backed script so each command that does
 * not need a process runs as shell code. A command that needs a real process
 * asks the host to write the guest program into /pb/bin and exec it. */
#define BOOT_PIPE_FD 9

extern int ish_pipe_pump(int fd, const char *root);
extern int ish_spawn_child(const char *budget_pipe, const char *root);

/* Longest single guest argument accepted from PB_ARGV_B64. */
#define ARGS_MAX 4096
#define MAX_ARGS 64

static int json_argv_decode(const char *json, char **argv_out, int cap) {
    int n = 0;
    const char *p = json;
    while (*p != '\0' && n < cap) {
        while (*p == ' ' || *p == ',' || *p == '[' || *p == ']' || *p == '\n')
            p++;
        if (*p != '"')
            break;
        p++;
        char *out = argv_out[n];
        size_t o = 0;
        while (*p != '\0' && *p != '"' && o < ARGS_MAX - 1) {
            if (*p == '\\' && p[1] != '\0') {
                p++;
                switch (*p) {
                    case 'n': out[o++] = '\n'; break;
                    case 't': out[o++] = '\t'; break;
                    default: out[o++] = *p;
                }
                p++;
                continue;
            }
            out[o++] = *p++;
        }
        if (*p == '"')
            p++;
        out[o] = '\0';
        n++;
    }
    return n;
}

#ifdef ISH_BOOT_ARGS
char **g_boot_argv;
int g_boot_argc;

void ish_set_boot_args(int argc, char **argv) {
    g_boot_argv = argv;
    g_boot_argc = argc;
}
#endif

int ish_boot_once(int argc, char *const argv[], const char *envp) {
    (void) argc;
    (void) argv;
    static char guest_cmd[1 << 16];
    static char *parts[MAX_ARGS];
    static char storage[MAX_ARGS][ARGS_MAX];
    static char *boot_argv[MAX_ARGS + 3];
    for (int i = 0; i < MAX_ARGS; i++)
        parts[i] = storage[i];

    const char *root = getenv("ISH_ROOTFS");
    const char *script = getenv("ISH_SHELL_SCRIPT");
    const char *argv_b64 = getenv("PB_ARGV_B64");
    int n = 0;

    if (argv_b64 != NULL) {
        static char decoded[1 << 18];
        int dn = b64_decode(argv_b64, (unsigned char *) decoded, sizeof(decoded) - 1);
        if (dn <= 0)
            return _EINVAL;
        decoded[dn] = '\0';
        n = json_argv_decode(decoded, parts, MAX_ARGS);
        if (n == 0)
            return _EINVAL;
    } else {
        FILE *f = script != NULL ? fopen(script, "r") : NULL;
        if (f == NULL) {
            fprintf(stderr, "ISH_SHELL_SCRIPT: cannot open %s: %s\n",
                    script != NULL ? script : "(unset)", strerror(errno));
            return 1;
        }
        size_t got = fread(guest_cmd, 1, sizeof(guest_cmd) - 1, f);
        fclose(f);
        guest_cmd[got] = '\0';
        parts[0] = (char *) "sh";
        parts[1] = (char *) "-c";
        parts[2] = guest_cmd;
        n = 3;
    }

    int offset = 0;
#ifdef ISH_BOOT_ARGS
    if (root != NULL && root[0] != '\0') {
        boot_argv[0] = (char *) "ish";
        boot_argv[1] = (char *) "-f";
        boot_argv[2] = (char *) root;
        offset = 3;
        for (int i = 0; i < n; i++)
            boot_argv[3 + i] = parts[i];
        boot_argv[3 + n] = NULL;
        ish_set_boot_args(n + 3, boot_argv);
    } else {
        for (int i = 0; i < n; i++)
            boot_argv[i] = parts[i];
        boot_argv[n] = NULL;
        ish_set_boot_args(n, boot_argv);
    }
#else
    (void) offset;
    (void) boot_argv;
    fprintf(stderr, "ish_boot_once: built without ISH_BOOT_ARGS; use the CLI\n");
    return _EINVAL;
#endif
    return xX_main_Xx(1, boot_argv, envp);
}

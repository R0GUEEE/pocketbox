// iOS host glue for the pocketbox runtime.
//
// One run = one `ish` process. The guest program is started from a script file
// the host writes (ISH_SHELL_SCRIPT), so guest argv never has to survive shell
// quoting.

#import <Foundation/Foundation.h>
#include <spawn.h>
#include <fcntl.h>
#include <sys/wait.h>
#include <signal.h>
#include <unistd.h>
#include <errno.h>
#include <string.h>
#include <stdlib.h>
#include <stdio.h>

#include "ish.h"

extern char **environ;

@interface ISHHost : NSObject
@end
@implementation ISHHost
@end

static NSString *ishBundleDir(void) {
    return [[NSBundle bundleForClass:[ISHHost class]] bundlePath];
}

static NSString *ishExecutablePath(void) {
    return [ishBundleDir() stringByAppendingPathComponent:@"ish"];
}

static char *cstr(NSString *s) {
    return strdup(s.UTF8String ?: "");
}

static char **buildEnv(NSArray<NSString *> *pairs) {
    char **env = calloc(pairs.count + 1, sizeof(char *));
    for (NSUInteger i = 0; i < pairs.count; i++)
        env[i] = cstr(pairs[i]);
    env[pairs.count] = NULL;
    return env;
}

static void freeEnv(char **env) {
    if (env == NULL) return;
    for (char **p = env; *p; p++) free(*p);
    free(env);
}

int ish_build_fakefs(const char *tarball, const char *dest) {
    NSString *tool = [[ishExecutablePath() stringByDeletingLastPathComponent]
                      stringByAppendingPathComponent:@"fakefsify"];
    char *argv[] = {cstr(tool), (char *)tarball, (char *)dest, NULL};
    pid_t pid;
    int status = 0;
    int rc = posix_spawn(&pid, tool.UTF8String, NULL, NULL, argv, environ);
    free(argv[0]);
    if (rc != 0)
        return 127;
    waitpid(pid, &status, 0);
    return WIFEXITED(status) ? WEXITSTATUS(status) : 128;
}

// Runs one guest command. Returns the exit status, or 126/128+n on failure.
//
// The script the guest executes is written here; it execs the guest path in
// $0 with the remaining arguments and prints an exit marker, because the guest
// runtime can die in teardown after a successful program run.
int ish_run_launch(ISHLaunchSpec *spec, double timeoutSeconds) {
    if (spec == nil || spec.rootfs == nil || spec.argv.count == 0)
        return 126;

    NSString *scriptPath = [NSTemporaryDirectory() stringByAppendingPathComponent:
                            [NSString stringWithFormat:@"pb-run-%d-%u.sh", getpid(),
                             (unsigned)arc4random()]];
    NSMutableString *script = [NSMutableString string];
    [script appendString:@"#!/bin/sh\n"];
    [script appendString:@"exec \"$0\" \"$@\"\n"];
    if (![script writeToFile:scriptPath atomically:YES encoding:NSUTF8StringEncoding error:NULL])
        return 126;

    NSMutableArray<NSString *> *envPairs = [NSMutableArray arrayWithArray:spec.environment ?: @[]];
    [envPairs addObject:[@"ISH_SHELL_SCRIPT=" stringByAppendingString:scriptPath]];
    [envPairs addObject:@"POCKETBOX_GUEST=1"];
    [envPairs addObject:@"PATH=/pb/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"];
    NSData *argvData = [NSJSONSerialization dataWithJSONObject:spec.argv options:0 error:NULL];
    [envPairs addObject:[@"PB_ARGV_B64=" stringByAppendingString:
                          [argvData base64EncodedStringWithOptions:0]]];
    char **env = buildEnv(envPairs);

    // The data root and this framework's bin directory are bind-mounted by the
    // guest entry point itself (see ISH_BIND_MOUNTS below), so nothing here
    // has to know the mount table format.
    NSArray<NSString *> *flags = @[@"-f", spec.rootfs];
    char **argv = calloc(flags.count + 2, sizeof(char *));
    argv[0] = cstr(@"ish");
    for (NSUInteger i = 0; i < flags.count; i++)
        argv[i + 1] = cstr(flags[i]);
    argv[flags.count + 1] = NULL;

    posix_spawn_file_actions_t actions;
    posix_spawn_file_actions_init(&actions);
    if (spec.stdinFD >= 0)
        posix_spawn_file_actions_adddup2(&actions, spec.stdinFD, 0);
    else
        posix_spawn_file_actions_addopen(&actions, 0, "/dev/null", O_RDONLY, 0);
    if (spec.stdoutFD >= 0)
        posix_spawn_file_actions_adddup2(&actions, spec.stdoutFD, 1);
    if (spec.stderrFD >= 0)
        posix_spawn_file_actions_adddup2(&actions, spec.stderrFD, 2);

    pid_t pid;
    int err = posix_spawn(&pid, ishExecutablePath().UTF8String, &actions, NULL, argv, env);
    posix_spawn_file_actions_destroy(&actions);

    int status = 0;
    if (err == 0) {
        if (timeoutSeconds > 0) {
            dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)(timeoutSeconds * NSEC_PER_SEC)),
                           dispatch_get_global_queue(QOS_CLASS_UTILITY, 0), ^{
                kill(pid, SIGKILL);
            });
        }
        waitpid(pid, &status, 0);
    }

    for (NSUInteger i = 0; i < flags.count + 2; i++) free(argv[i]);
    free(argv);
    freeEnv(env);
    [[NSFileManager defaultManager] removeItemAtPath:scriptPath error:NULL];

    if (err != 0)
        return 126;
    if (WIFSIGNALED(status))
        return 128 + WTERMSIG(status);
    return WEXITSTATUS(status);
}

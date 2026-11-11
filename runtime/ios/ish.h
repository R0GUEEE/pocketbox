// Public interface of the embedded Linux guest runtime.
#ifndef POCKETBOX_ISH_H
#define POCKETBOX_ISH_H

#import <Foundation/Foundation.h>

NS_ASSUME_NONNULL_BEGIN

/// One guest command invocation.
@interface ISHLaunchSpec : NSObject
/// Host path of the fakefs rootfs (directory containing data/ and meta.db).
@property (nonatomic, copy) NSString *rootfs;
/// Guest argv: argv[0] is the guest binary path, the rest its arguments.
@property (nonatomic, copy) NSArray<NSString *> *argv;
/// KEY=VALUE entries for the guest environment. POCKETBOX_DATA and the bind
/// mounts are added by the runtime.
@property (nonatomic, copy) NSArray<NSString *> *environment;
/// Host descriptors for stdio. -1 means /dev/null (stdin) or inherit.
@property (nonatomic, assign) int stdinFD;
@property (nonatomic, assign) int stdoutFD;
@property (nonatomic, assign) int stderrFD;
@end

/// Builds a fakefs image at `dest` from a rootfs tarball.
int ish_build_fakefs(const char *tarball, const char *dest);

/// Runs `spec` and returns its exit status. Blocks the calling thread; call it
/// from a background queue. A non-positive timeout disables the watchdog.
int ish_run_launch(ISHLaunchSpec *spec, double timeoutSeconds);

NS_ASSUME_NONNULL_END

#endif

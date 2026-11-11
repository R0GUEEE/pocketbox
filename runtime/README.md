# Embedded runtime: build status and what blocks it

The app runs containers inside an arm64 Linux guest. The runtime is
rcarmo/ios-linuxkit (iSH, ARM64) plus the engine in this repository. This page
records exactly what is proven, what is built, and what stopped the iOS build on
the build device, so the next person does not repeat the trail.

## Layout

```
ish (arm64-apple-ios, Mach-O)  ── runs one guest program image per launch
  └─ Alpine 3.24 fakefs at /pb/rootfs/alpine.pbfs
       └─ /pb/bin/pocketbox (linux/arm64, static)  ── the engine
            └─ pkg/guestrunner writes /pb/host/run/<id>.json
                 └─ the app's host process launches the container program
```

The engine never forks inside the guest: it asks the host for a process and
polls a status file. Network work goes the same way through `pkg/netg`
(`/pb/host/net/<id>.req` / `.resp`), which is verified end to end.

## Proven working (on this device's iSH build)

| item | evidence |
|---|---|
| iSH builds on Alpine/musl and boots an Alpine 3.24 fakefs | `ish -f rootfs /bin/sh -c 'echo A'` → `A` |
| Engine pull → layers → rootfs, real registries | busybox, alpine, python:3.12-alpine (4 layers) |
| Pull through the file-based host network bridge | `TestPullViaBridge` |
| Engine CLI and Docker API on a Linux host | `ps/images/run/logs/serve`, `curl /_ping /version /containers` |
| fakefsify with an in-tree libarchive shim | shim-built fakefs is byte-identical to the libarchive one (515 paths, identical meta.db) |

## Built for iOS (arm64-apple-ios, iPhoneOS 16.5 SDK)

* 88 of 95 runtime translation units compile for the iOS ABI, including the
  full guest kernel, the emulator core and `main.c`.
* `libfakefs.a`, static sqlite3 and zlib are built for the same target.
* `fakefsify` compiles and links everything except libsystem symbols.

## Blocked on the build device

The only Mach-O linker available here is `ld64.lld` 19, and it cannot resolve
the SDK's stub reexports: `libSystem.tbd` reexports `libsystem_c.dylib`, whose
symbols (`___errno_location`, `_stderr`) come back undefined. A macOS runner
with a real `ld` does not have this problem, so linking and IPA packaging belong
on CI.

Caveats that CI must satisfy:

* `platform/linux.c` and `platform/native_fault.c` are Linux host files and are
  excluded; the Darwin counterpart (`platform/darwin.c`) is part of the target's
  source list.
* The iOS build needs `-DGUEST_ARM64=1`, `-DENGINE_ASBESTOS=1`, and
  `_XOPEN_SOURCE` for `ucontext.h` in `main.c`.
* `fs/dev.h` uses `major()`/`minor()` which iOS only declares with Darwin
  extensions; the guard in the patch keeps the fake encoding exact.

## Runtime behaviour to know before testing on device

1. **One guest process per launch.** Anything that starts a second guest process
   (`sh -c '$(id)'`, a pipe) kills the emulator here.
2. **Teardown crash.** A guest program often exits cleanly and then the
   emulator crashes with SIGSEGV (139); the exit status printed by the program
   is authoritative.
3. **Thread start races.** Roughly half of guest runs fail with
   `task_start` EPERM inside the guest. Retry once after a short delay.
4. **Fork from the host.** A host `fork()` can make every later exec fail; do
   not fork from the app before the guest runs.

The debug switches added by the patches (`ISH_LOG=strace`, `ISH_TRACE_EXEC`,
`ISH_TRACE_BOOT`, `ISH_SHELL_SCRIPT`, `PB_ARGV_B64`, `ISH_ROOTFS`) are what made
the above diagnosable.

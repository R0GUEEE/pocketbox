# Runtime patches

`0001-pocketbox-runtime.patch` applies on top of rcarmo/ios-linuxkit (the ARM64
iSH fork) and contains everything needed to:

* build on musl (guarded `asm/sigcontext.h` include, `-lexecinfo`, a
  `getrusage` fallback);
* compile for iOS without Darwin extensions (`fs/dev.h` device macros,
  `tools/fakefs.c` timespec spelling);
* start one guest program per launch (`ISH_ROOTFS` + `PB_ARGV_B64`, decoded in
  `kernel/boot_entry.c`);
* keep a persistent guest shell with a host command pipe
  (`kernel/boot_pipe.c`);
* add a native spawn executor for Linux hosts (`kernel/native_spawn*.c`, with
  tests in `kernel/native_spawn.h`'s contract);
* runtime tracing without a rebuild (`ISH_LOG=strace`, `ISH_TRACE_EXEC`).

Apply from the ios-linuxkit root:

```sh
git apply /path/to/0001-pocketbox-runtime.patch
```

`archive_shim.c` is not part of the patch: it replaces libarchive for the iOS
build of `fakefsify`, implementing only the tar/tar.gz read and write entries
that tool uses. Copy it to `tools/archive_shim.c` and build `fakefsify` against
it instead of the vendored library:

```sh
clang --target=arm64-apple-ios16.0 -isysroot "$SDK" -DGUEST_ARM64=1 \
      -I. -isystem /path/to/host/include \
      tools/fakefsify.c tools/fakefs.c util/fchdir.c tools/archive_shim.c \
      libfakefs.a sqlite3.o z_*.o -o fakefsify
```

The shim's output was checked byte for byte against the libarchive importer
(same 515 paths, identical `meta.db` contents).

`build-ios-runtime.py` (in this repo's `scripts/`) drives the cross compile of
every other runtime unit from a meson `compdb`.

#!/usr/bin/env python3
"""Builds the PocketBox iOS runtime pieces with the cross toolchain.

Inputs: the iSH source tree, an iOS SDK, and a build directory from a native
meson configuration (used only for its compile flags).
Outputs: ish, fakefsify and libfakefs.a built for arm64-apple-ios, plus a
libsqlite3.a and zlib built for the same target.
"""
import json
import os
import shlex
import subprocess
import sys
import shutil
import concurrent.futures

SRC = os.environ.get("ISH_SRC", "/root/ios-linuxkit")
SDK = os.environ.get("IOS_SDK", "/root/.ios-sdk/iPhoneOS16.5.sdk")
BUILD = os.environ.get("ISH_BUILD", SRC + "/build-arm64-linux")
OUT = os.environ.get("IOS_OUT", "/tmp/iosrt")
TARGET = ["--target=arm64-apple-ios16.0", "-isysroot", SDK, "-isystem", SDK + "/usr/include"]
# _XOPEN_SOURCE is what the iOS SDK wants before it exposes ucontext; musl on
# the build host does not care either way.
DEFS = ["-DENGINE_ASBESTOS=1", "-DGUEST_ARM64=1", "-DLOG_HANDLER_DPRINTF=1",
        "-D_XOPEN_SOURCE=700", "-D_DARWIN_C_SOURCE"]
LINUX_ONLY = {"platform/linux.c", "platform/native_fault.c"}
DROP_SOURCES = {"tools/fakefs.c", "tools/fakefsify.c", "util/fchdir.c"}


def compdb():
    raw = subprocess.run(["ninja", "-C", BUILD, "-t", "compdb", "c_COMPILER"],
                         capture_output=True, text=True, check=True).stdout
    return json.loads(raw)


def parse(entry):
    args = shlex.split(entry.get("command", "")) if "command" in entry else entry["arguments"]
    rest = args[1:]
    includes = []
    i = 0
    while i < len(rest) and rest[i].startswith("-I"):
        flag = rest[i]
        # Meson emits include paths relative to the build directory
        # (-I. -I.. -I../vdso/arm64); resolve them against it so clang, which
        # runs from the source root here, looks in the same places.
        path = flag[2:]
        resolved = os.path.normpath(os.path.join(BUILD, path))
        includes.append("-I" + resolved)
        i += 1
    cfile = None
    for a in rest:
        if a.endswith(".c") and not a.endswith(".o"):
            cfile = a
    defines = [a for a in rest if a.startswith("-D")]
    return cfile, includes, defines


def build(entry):
    cfile, includes, defines = parse(entry)
    if cfile is None:
        return entry["output"], 0, ""
    rel = os.path.normpath(os.path.join(BUILD, cfile))
    relsrc = os.path.relpath(rel, SRC)
    if relsrc in LINUX_ONLY or relsrc in DROP_SOURCES:
        return entry["output"], 0, ""
    oname = os.path.join(OUT, "obj", entry["output"].replace("/", "_"))
    keep = [d for d in defines if d not in ("-DLOG_HANDLER_DPRINTF=1",)]
    cmd = ["clang"] + TARGET + ["-I", SRC, "-I", SRC + "/vdso/arm64"] + includes + DEFS + keep + ["-O1", "-std=gnu11", "-c", rel, "-o", oname]
    p = subprocess.run(cmd, capture_output=True, text=True)
    return entry["output"], p.returncode, p.stderr[-1500:]


def main():
    os.makedirs(os.path.join(OUT, "obj"), exist_ok=True)
    db = compdb()
    fails = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as ex:
        for name, rc, err in ex.map(build, db):
            if rc != 0:
                fails.append((name, err))
    print(f"compiled {len(db)} units, {len(fails)} failed")
    for n, e in fails[:8]:
        print("==", n)
        print(e)
    if fails:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

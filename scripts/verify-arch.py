#!/usr/bin/env python3
"""Verify that the cross-compiled artifacts are what we think they are.

Why this exists
---------------
`go build` happily produces a *windows* binary into `dist/lan-share` if the
GOOS/GOARCH environment variables did not take effect (e.g. they were clobbered
because the .bat file was mis-decoded, or a stale value leaked from the shell).
Nothing in the build itself notices. The failure only shows up on the router,
as `sh: ./lan-share: not found` / `Exec format error` -- after you have already
spent a round trip on it.

So: never trust the build. Read the first bytes back and assert.

Checks
------
  dist/lan-share       must be ELF64, little-endian, e_machine=0xB7 (AArch64)
  dist/lan-share.exe   must be PE/MZ (windows/amd64)  [optional]

Exit code 0 = all good, 1 = something is wrong (build script aborts).
"""

import os
import struct
import sys

ELF_MAGIC = b"\x7fELF"
EM_AARCH64 = 0xB7
EM_X86_64 = 0x3E
PE_MAGIC = b"MZ"

# (relative path, expected human name, expected e_machine or None for PE)
TARGETS = [
    (os.path.join("dist", "lan-share"), "linux/arm64", EM_AARCH64),
    (os.path.join("dist", "lan-share.exe"), "windows/amd64", None),
]

ok = True

for path, want, want_machine in TARGETS:
    label = "%-22s" % path

    if not os.path.exists(path):
        # the .exe is a convenience debug build; treat a miss as a warning
        if want_machine is None:
            print("  [warn] %s missing (debug build skipped)" % label)
            continue
        print("  [FAIL] %s missing" % label)
        ok = False
        continue

    with open(path, "rb") as fh:
        head = fh.read(20)

    size_mb = os.path.getsize(path) / 1024 / 1024

    if head[:4] == ELF_MAGIC:
        ei_class = head[4]        # 1 = 32-bit, 2 = 64-bit
        ei_data = head[5]         # 1 = little-endian
        e_machine = struct.unpack("<H", head[18:20])[0]

        problems = []
        if ei_class != 2:
            problems.append("not 64-bit (ei_class=%d)" % ei_class)
        if ei_data != 1:
            problems.append("not little-endian (ei_data=%d)" % ei_data)
        if e_machine != want_machine:
            problems.append("e_machine=0x%X, want 0x%X" % (e_machine, want_machine))

        if problems:
            print("  [FAIL] %s is ELF but wrong: %s" % (label, "; ".join(problems)))
            ok = False
        else:
            print("  [ OK ] %s ELF64 LE e_machine=0x%X (%s, %.2f MB)"
                  % (label, e_machine, want, size_mb))

    elif head[:2] == PE_MAGIC:
        if want_machine is None:
            print("  [ OK ] %s PE/MZ (%s, %.2f MB)" % (label, want, size_mb))
        else:
            # This is the dangerous case: a Windows binary sitting at the path
            # you are about to scp to the router.
            print("  [FAIL] %s is a Windows PE binary, not a Linux ELF!" % label)
            print("         -> GOOS/GOARCH did not take effect during the build.")
            print("         -> Do NOT copy this file to the router.")
            ok = False

    else:
        print("  [FAIL] %s unrecognised format (first bytes: %s)"
              % (label, head[:4].hex(" ")))
        ok = False

if not ok:
    print("")
    print("Architecture check failed. The artifacts above must not be deployed.")
    sys.exit(1)

print("")
print("Architecture check passed.")
sys.exit(0)

#!/bin/sh
# Builds and runs the host-side test for the Writer protocol core.
#
# The protocol layer (src/diskproto.c) has no Pico/TinyUSB dependencies, so it
# compiles with the host compiler against a fake flash drive.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)

cc=${CC:-cc}
out=${1:-/tmp/dh_diskproto_test}

$cc -std=c11 -O1 -g -Wall -Wextra -Wno-unused-parameter \
    -I "$root/src/include" \
    "$here/test_diskproto.c" "$root/src/diskproto.c" \
    -o "$out"

"$out"
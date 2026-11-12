#!/bin/sh
# Remove the LLVM bitcode sections from the prebuilt Bitwarden SDK library.
#
# Go 1.27.4 and 1.26.11 (CVE-2026-97038) reject static libraries in
# "#cgo LDFLAGS" that contain link-time optimization (LTO) sections. The
# Bitwarden Go SDK (github.com/bitwarden/sdk-go/v2) ships libbitwarden_c.a with
# embedded LLVM bitcode, so cgo builds fail with "disallowed LTO section".
# Upstream issue: https://github.com/bitwarden/sdk-sm/issues/1592
#
# The bitcode is only an input for LTO. Each object file also contains the
# compiled machine code, and the linker uses only that code. Thus the removal
# does not change the linked binary.
#
# The script changes the library in the Go module cache in place. It changes
# only the library for the current GOOS and GOARCH. It does nothing when the
# library has no LTO sections, so you can run it again safely.
#
# Set OBJCOPY to use a specific objcopy binary. Linux uses GNU objcopy from
# binutils by default. macOS needs LLVM objcopy, for example from
# "brew install llvm" or "rust-objcopy" from the Rust llvm-tools component.
set -eu

module=github.com/bitwarden/sdk-go/v2

goos=$(go env GOOS)
goarch=$(go env GOARCH)

case "$goos/$goarch" in
  linux/amd64) platform=linux-x64 ;;
  linux/arm64) platform=linux-arm64 ;;
  darwin/amd64) platform=darwin-x64 ;;
  darwin/arm64) platform=darwin-arm64 ;;
  *)
    echo "strip-bitwarden-lto: no Bitwarden SDK library to process for $goos/$goarch"
    exit 0
    ;;
esac

# Without the inspect tool, has_lto cannot find LTO sections and the script
# would skip a library that still needs changes.
case "$goos" in
  linux) inspect_bin=readelf ;;
  darwin) inspect_bin=otool ;;
esac

if ! command -v "$inspect_bin" >/dev/null 2>&1; then
  echo "strip-bitwarden-lto: $inspect_bin not found" >&2
  exit 1
fi

go mod download "$module"
moddir=$(go list -m -f '{{.Dir}}' "$module")

if [ -z "$moddir" ]; then
  echo "strip-bitwarden-lto: cannot find $module in the Go module cache" >&2
  exit 1
fi

libdir="$moddir/internal/cinterface/lib/$platform"
lib="$libdir/libbitwarden_c.a"

if [ ! -f "$lib" ]; then
  echo "strip-bitwarden-lto: $lib does not exist" >&2
  exit 1
fi

# These patterns match the sections and segments that the go command rejects.
has_lto() {
  case "$goos" in
    linux)
      readelf -S -W "$1" 2>/dev/null |
        grep -q -E '\] (\.llvmbc|\.llvmcmd|\.llvm\.lto|\.gnu\.lto_|\.gnu\.debuglto_)|LLVM_LTO|LOOS\+0xfff4c0c'
      ;;
    darwin)
      otool -l "$1" 2>/dev/null | grep -q -E 'segname __(LLVM|GNU_LTO)'
      ;;
  esac
}

if ! has_lto "$lib"; then
  echo "strip-bitwarden-lto: $lib has no LTO sections"
  exit 0
fi

objcopy_bin=${OBJCOPY:-}

if [ -z "$objcopy_bin" ]; then
  case "$goos" in
    linux)
      objcopy_bin=objcopy
      ;;
    darwin)
      for candidate in llvm-objcopy /opt/homebrew/opt/llvm/bin/llvm-objcopy /usr/local/opt/llvm/bin/llvm-objcopy; do
        if command -v "$candidate" >/dev/null 2>&1; then
          objcopy_bin=$candidate
          break
        fi
      done
      ;;
  esac
fi

if [ -z "$objcopy_bin" ] || ! command -v "$objcopy_bin" >/dev/null 2>&1; then
  echo "strip-bitwarden-lto: objcopy not found." >&2

  if [ "$goos" = darwin ]; then
    echo "Install LLVM with \"brew install llvm\" or set OBJCOPY to an LLVM objcopy binary." >&2
  else
    echo "Install binutils or set OBJCOPY to an objcopy binary." >&2
  fi

  exit 1
fi

# The Go module cache is read-only. objcopy writes a temporary file next to the
# library, so the directory and the file must be writable for a short time.
restore_dir=false
restore_lib=false

restore_permissions() {
  if [ "$restore_lib" = true ]; then
    chmod a-w "$lib"
  fi

  if [ "$restore_dir" = true ]; then
    chmod a-w "$libdir"
  fi
}

trap restore_permissions EXIT

if [ ! -w "$libdir" ]; then
  chmod u+w "$libdir"
  restore_dir=true
fi

if [ ! -w "$lib" ]; then
  chmod u+w "$lib"
  restore_lib=true
fi

echo "strip-bitwarden-lto: removing LTO sections from $lib"

case "$goos" in
  linux)
    "$objcopy_bin" --remove-section=.llvmbc --remove-section=.llvmcmd "$lib"
    ;;
  darwin)
    "$objcopy_bin" --remove-section=__LLVM,__bitcode --remove-section=__LLVM,__cmdline "$lib"
    ;;
esac

if has_lto "$lib"; then
  echo "strip-bitwarden-lto: $lib still has LTO sections" >&2
  exit 1
fi

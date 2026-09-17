#!/usr/bin/env bash
# musl_test.sh - the musllinux wheels, checked before they reach PyPI.
#
# pip on musl (Alpine) accepts only musllinux and plain linux tags, so a release
# with manylinux wheels alone answers `pip install enola-cli` there with "no
# matching distribution". The manylinux binary cannot simply be reused either: it
# asks for /lib/ld-linux-*.so.1, which musl does not have, and dies with "not
# found".
#
# This builds the pip binary the way the release workflow does (the same
# musllinux_1_2 image, the official Go toolchain mounted at /usr/local/go), then
# checks four things:
#
#   1. the binary asks for the musl loader and links nothing but musl libc
#   2. pip installs the wheel on stock Alpine and it runs a real snapshot, with
#      files for the cgo tree-sitter grammars (Swift, Dart) in it
#   3. from a directory holding both a manylinux and a musllinux wheel, Alpine
#      picks musllinux and a glibc distro picks manylinux
#   4. the oldest Alpine claimed (musl 1.2.3) still runs it
#
# Usage:  packaging/pypi/musl_test.sh
# Env:    ENOLA_WHEEL_WORK_MUSL   work directory (must be under $HOME)
#         DOCKER_PLATFORM         linux/amd64 or linux/arm64, default the host

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# Under $HOME because colima does not mount $TMPDIR into its VM, and a bind
# mount of a path the VM cannot see silently becomes an empty directory.
WORK="${ENOLA_WHEEL_WORK_MUSL:-$HOME/.cache/enola-wheel-musl}"
VERSION="0.0.0"
GOVER="$(awk '/^go /{print $2; exit}' "$REPO_ROOT/go.mod")"

HOST_ARCH="$(uname -m)"
case "${DOCKER_PLATFORM:-}" in
  linux/amd64) GOARCH=amd64; WHEEL_ARCH=x86_64 ;;
  linux/arm64) GOARCH=arm64; WHEEL_ARCH=aarch64 ;;
  "")
    case "$HOST_ARCH" in
      arm64|aarch64) GOARCH=arm64; WHEEL_ARCH=aarch64 ;;
      x86_64)        GOARCH=amd64; WHEEL_ARCH=x86_64 ;;
      *) echo "unsupported host arch $HOST_ARCH" >&2; exit 1 ;;
    esac
    ;;
  *) echo "DOCKER_PLATFORM must be linux/amd64 or linux/arm64" >&2; exit 1 ;;
esac
PLATFORM="linux/${GOARCH}"

BUILD_IMAGE="quay.io/pypa/musllinux_1_2_${WHEEL_ARCH}"
MUSL_TAG="musllinux_1_2_${WHEEL_ARCH}"
MANY_TAG="manylinux_2_17_${WHEEL_ARCH}.manylinux2014_${WHEEL_ARCH}"

# "image|note". Alpine 3.17 is the oldest release on musl 1.2.x still in the
# pypa images' lineage, so it is the floor the 1_2 tag has to hold at.
ALPINE_IMAGES=(
  "alpine:3.22|current"
  "alpine:3.17|musl 1.2.3, the floor"
)
GLIBC_IMAGE="python:3.12-slim-bookworm"

pass_count=0; fail_count=0
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { printf '   \033[32mPASS\033[0m %s\n' "$*"; pass_count=$((pass_count + 1)); }
bad()  { printf '   \033[31mFAIL\033[0m %s\n' "$*"; fail_count=$((fail_count + 1)); }
note() { printf '   ---- %s\n' "$*"; }

rm -rf "$WORK/bin" "$WORK/dist" "$WORK/mixed" "$WORK/scripts"
mkdir -p "$WORK/go" "$WORK/bin" "$WORK/dist" "$WORK/mixed" "$WORK/scripts"

echo "mount-is-real" > "$WORK/scripts/.mount_probe"
PROBE="$(docker run --rm -v "$WORK/scripts:/probe:ro" alpine cat /probe/.mount_probe 2>&1 || true)"
if [ "$PROBE" != "mount-is-real" ]; then
  echo "error: $WORK is not visible inside containers (got: $PROBE)." >&2
  echo "Set ENOLA_WHEEL_WORK_MUSL to a directory under \$HOME." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
step "Fetch go${GOVER}.linux-${GOARCH}, the toolchain setup-go mounts in CI"

GOTAR="$WORK/go/go${GOVER}.linux-${GOARCH}.tar.gz"
if [ ! -f "$GOTAR" ]; then
  curl -fsSL -o "$GOTAR" "https://go.dev/dl/go${GOVER}.linux-${GOARCH}.tar.gz"
fi
if [ ! -x "$WORK/go/root/go/bin/go" ]; then
  rm -rf "$WORK/go/root"; mkdir -p "$WORK/go/root"
  tar -C "$WORK/go/root" -xzf "$GOTAR"
fi
ok "go${GOVER}.linux-${GOARCH}"

# ---------------------------------------------------------------------------
step "Build in ${BUILD_IMAGE}"

docker volume create enola-wheel-musl-gomod >/dev/null
LDFLAGS="-s -w -X github.com/enola-labs/enola/internal/version.Version=${VERSION}"
PIPFLAGS="$LDFLAGS -X github.com/enola-labs/enola/internal/version.InstallMethod=pip"

if docker run --rm --platform "$PLATFORM" \
    -v "$REPO_ROOT:/src:ro" \
    -v "$WORK/go/root/go:/usr/local/go:ro" \
    -v "$WORK/bin:/out" \
    -v enola-wheel-musl-gomod:/root/go/pkg/mod \
    -w /src \
    -e CGO_ENABLED=1 -e GOOS=linux -e "GOARCH=$GOARCH" -e GOFLAGS=-buildvcs=false \
    -e "PIPFLAGS=$PIPFLAGS" \
    "$BUILD_IMAGE" \
    /bin/sh -c '/usr/local/go/bin/go build -ldflags "$PIPFLAGS" -o /out/enola ./cmd/enola &&
                echo INTERP_BEGIN && readelf -l /out/enola | grep -o "interpreter: [^]]*" &&
                echo NEEDED_BEGIN && readelf -d /out/enola | grep NEEDED' \
    >"$WORK/build.log" 2>&1; then
  ok "built"
else
  bad "build failed"
  tail -20 "$WORK/build.log" | sed 's/^/        /'
  exit 1
fi

INTERP="$(grep -o 'interpreter: .*' "$WORK/build.log" | sed 's/interpreter: //')"
NEEDED="$(grep -o 'Shared library: \[[^]]*\]' "$WORK/build.log" | sed 's/.*\[//; s/\]//' | tr '\n' ' ')"
note "interpreter: $INTERP"
note "needed: $NEEDED"
case "$INTERP" in
  /lib/ld-musl-*) ok "asks for the musl loader" ;;
  *) bad "interpreter is not musl: $INTERP" ;;
esac
OUTSIDE=""
for lib in $NEEDED; do
  case "$lib" in libc.musl-*) ;; *) OUTSIDE="$OUTSIDE $lib" ;; esac
done
if [ -z "$OUTSIDE" ]; then ok "links only musl libc"; else bad "links more than musl libc:$OUTSIDE"; fi

# ---------------------------------------------------------------------------
step "Wheels"

python3 "$REPO_ROOT/packaging/pypi/build_wheel.py" --binary "$WORK/bin/enola" \
  --version "$VERSION" --platform-tag "$MUSL_TAG" --outdir "$WORK/dist" >/dev/null
MUSL_WHEEL="enola_cli-${VERSION}-py3-none-${MUSL_TAG}.whl"
[ -f "$WORK/dist/$MUSL_WHEEL" ] && ok "$MUSL_WHEEL" || bad "no $MUSL_WHEEL"

# The selection check needs a manylinux wheel next to it. Only its filename is
# ever read (pip download, never run), so the musl binary inside is fine.
cp "$WORK/dist/$MUSL_WHEEL" "$WORK/mixed/"
python3 "$REPO_ROOT/packaging/pypi/build_wheel.py" --binary "$WORK/bin/enola" \
  --version "$VERSION" --platform-tag "$MANY_TAG" --outdir "$WORK/mixed" >/dev/null
ok "mixed directory: $(ls "$WORK/mixed" | tr '\n' ' ')"

cat > "$WORK/scripts/alpine_run.sh" <<'INNER'
#!/bin/sh
set -eu
apk add --no-cache python3 py3-pip git >/dev/null
python3 -m venv /venv
. /venv/bin/activate
pip install --quiet --no-cache-dir --no-index --find-links /dist enola-cli
echo "VERSION: $(enola --version 2>&1)"

mkdir -p /r/app/Sources /r/lib /r/web /r/dart
cd /r
git init -q; git config user.email t@t; git config user.name t
printf 'module example.com/r\ngo 1.22\n' > go.mod
printf 'package main\nimport "example.com/r/lib"\nfunc main(){ lib.Hello() }\n' > app/main.go
printf 'package lib\nfunc Hello(){}\n' > lib/lib.go
printf 'import Foundation\nstruct User { let name: String }\nclass Svc { func get() -> User { User(name: "x") } }\n' > app/Sources/Svc.swift
printf 'class Repo {\n  String find() => "x";\n}\n' > dart/repo.dart
printf 'export function f(): number { return 1 }\n' > web/a.ts
printf 'def g():\n    return 1\n' > web/b.py
git add -A; git commit -qm init
enola baseline pin /r >/dev/null 2>&1
for f in app/Sources/Svc.swift dart/repo.dart web/a.ts web/b.py app/main.go; do
  echo "FACTS $f $(grep -c "\"file\":\"$f\"" /r/.enola/facts.jsonl || true)"
done
INNER

cat > "$WORK/scripts/select.sh" <<'INNER'
#!/bin/sh
set -eu
command -v pip >/dev/null 2>&1 || { apk add --no-cache py3-pip >/dev/null; python3 -m venv /venv; . /venv/bin/activate; }
pip download --quiet --no-cache-dir --no-deps --no-index --find-links /dist -d /tmp/d enola-cli
echo "PICKED: $(ls /tmp/d)"
INNER

# ---------------------------------------------------------------------------
for entry in "${ALPINE_IMAGES[@]}"; do
  IMAGE="${entry%%|*}"; NOTE="${entry#*|}"
  step "pip install and snapshot on ${IMAGE} (${NOTE})"

  set +e
  OUT="$(docker run --rm --platform "$PLATFORM" -v "$WORK/dist:/dist:ro" \
    -v "$WORK/scripts:/scripts:ro" "$IMAGE" sh /scripts/alpine_run.sh 2>&1)"
  RC=$?
  set -e
  if [ "$RC" -ne 0 ]; then
    bad "install or run failed on $IMAGE"
    echo "$OUT" | tail -10 | sed 's/^/        /'
    continue
  fi
  GOT="$(echo "$OUT" | grep '^VERSION:' | sed 's/VERSION: //')"
  case "$GOT" in
    *"$VERSION"*) ok "installed by pip and runs: $GOT" ;;
    *) bad "unexpected version output: '$GOT'" ;;
  esac
  echo "$OUT" | grep '^FACTS ' | while read -r _ f n; do
    if [ "$n" -gt 0 ]; then ok "$f: $n facts"; else bad "$f: no facts"; fi
  done
done

# ---------------------------------------------------------------------------
step "pip picks the right wheel from a mixed set"

for pair in "alpine:3.22|$MUSL_TAG" "$GLIBC_IMAGE|$MANY_TAG"; do
  IMAGE="${pair%%|*}"; WANT="${pair#*|}"
  PICKED="$(docker run --rm --platform "$PLATFORM" -v "$WORK/mixed:/dist:ro" \
    -v "$WORK/scripts:/scripts:ro" "$IMAGE" \
    sh -c 'command -v python3 >/dev/null || apk add --no-cache python3 >/dev/null; sh /scripts/select.sh' 2>&1 \
    | grep '^PICKED:' | sed 's/PICKED: //' || true)"
  if [ "$PICKED" = "enola_cli-${VERSION}-py3-none-${WANT}.whl" ]; then
    ok "$IMAGE -> $WANT"
  else
    bad "$IMAGE picked '${PICKED:-nothing}', wanted $WANT"
  fi
done

# ---------------------------------------------------------------------------
printf '\n\033[1m== Summary\033[0m\n'
printf '   %d passed, %d failed\n' "$pass_count" "$fail_count"
printf '   work directory: %s\n' "$WORK"
[ "$fail_count" -eq 0 ]

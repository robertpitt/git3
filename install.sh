#!/bin/sh
set -eu
umask 077

OWNER_REPO=${GIT3_REPOSITORY:-robertpitt/git3}
INSTALL_DIR=${GIT3_INSTALL_DIR:-"$HOME/.local/bin"}
TMP_ROOT=${TMPDIR:-/tmp}
WORK_DIR=$(mktemp -d "$TMP_ROOT/git3-install.XXXXXXXX")
trap 'rm -rf "$WORK_DIR"' EXIT HUP INT TERM

case $(uname -s) in Linux) OS=linux ;; Darwin) OS=darwin ;; *) echo "git3: unsupported OS" >&2; exit 1 ;; esac
case $(uname -m) in x86_64|amd64) ARCH=amd64 ;; arm64|aarch64) ARCH=arm64 ;; *) echo "git3: unsupported architecture" >&2; exit 1 ;; esac

fetch() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then wget -qO "$2" "$1"
  else echo "git3: curl or wget is required" >&2; exit 1
  fi
}

if [ "${GIT3_VERSION:-}" ]; then VERSION=$GIT3_VERSION
else
  if command -v curl >/dev/null 2>&1; then
    FINAL=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$OWNER_REPO/releases/latest")
    VERSION=${FINAL##*/tag/}
  else
    FINAL=$(wget -qO /dev/null --server-response "https://github.com/$OWNER_REPO/releases/latest" 2>&1 | awk '/^  Location:/{v=$2} END{gsub("\\r", "", v); print v}')
    VERSION=${FINAL##*/tag/}
  fi
fi
case $VERSION in v*.*.*) ;; *) echo "git3: invalid release version $VERSION" >&2; exit 1 ;; esac
SEMVER=${VERSION#v}; OLD_IFS=$IFS; IFS=.
# shellcheck disable=SC2086
set -- $SEMVER
IFS=$OLD_IFS
[ "$#" = 3 ] || { echo "git3: invalid release version $VERSION" >&2; exit 1; }
for component in "$@"; do case $component in ''|*[!0-9]*) echo "git3: invalid release version $VERSION" >&2; exit 1 ;; esac; done

ASSET="git3_${VERSION#v}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/$OWNER_REPO/releases/download/$VERSION"
fetch "$BASE/$ASSET" "$WORK_DIR/$ASSET"
fetch "$BASE/checksums.txt" "$WORK_DIR/checksums.txt"
EXPECTED=$(awk -v f="$ASSET" '$2==f || $2=="*"f {print $1}' "$WORK_DIR/checksums.txt")
[ -n "$EXPECTED" ] || { echo "git3: asset missing from checksums" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then ACTUAL=$(sha256sum "$WORK_DIR/$ASSET" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then ACTUAL=$(shasum -a 256 "$WORK_DIR/$ASSET" | awk '{print $1}')
else echo "git3: sha256sum or shasum is required" >&2; exit 1
fi
[ "$ACTUAL" = "$EXPECTED" ] || { echo "git3: checksum verification failed" >&2; exit 1; }

tar -tzf "$WORK_DIR/$ASSET" >"$WORK_DIR/names"
[ "$(wc -l <"$WORK_DIR/names" | tr -d ' ')" = 3 ] || { echo "git3: unexpected archive contents" >&2; exit 1; }
for n in git3 LICENSE NOTICE; do grep -qx "$n" "$WORK_DIR/names" || { echo "git3: missing $n" >&2; exit 1; }; done
tar -tvzf "$WORK_DIR/$ASSET" | awk '$1 !~ /^-/ {exit 1}' || { echo "git3: links/devices are forbidden" >&2; exit 1; }
tar -xzf "$WORK_DIR/$ASSET" -C "$WORK_DIR" -- git3 LICENSE NOTICE
chmod 755 "$WORK_DIR/git3"
mkdir -p "$INSTALL_DIR"
TARGET_TMP="$INSTALL_DIR/.git3.$$.tmp"
cp "$WORK_DIR/git3" "$TARGET_TMP"
chmod 755 "$TARGET_TMP"
mv -f "$TARGET_TMP" "$INSTALL_DIR/git3"
for n in git-s3 git-remote-s3; do
  rm -f "$INSTALL_DIR/$n"
  if ! (cd "$INSTALL_DIR" && ln -s git3 "$n") 2>/dev/null; then cp "$INSTALL_DIR/git3" "$INSTALL_DIR/$n"; fi
done
case :$PATH: in *:"$INSTALL_DIR":*) ;; *) echo "git3: add $INSTALL_DIR to PATH" >&2 ;; esac
"$INSTALL_DIR/git3" version
echo "Verify release provenance with the checksums Sigstore bundle and GitHub artifact attestation."

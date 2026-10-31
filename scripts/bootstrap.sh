#!/usr/bin/env bash
# Run from a reviewed project checkout. Installs no dependencies or services.
set -Eeuo pipefail
umask 077
PROJECT=$(cd -- "$(dirname -- "$0")/.." && pwd -P)
VERSION=$(cat "$PROJECT/VERSION")
OUTPUT=
while (($#)); do
    case "$1" in
        --version) VERSION=${2:?}; shift 2 ;;
        --output) OUTPUT=${2:?}; shift 2 ;;
        *) echo 'Use scripts/bootstrap.sh --output NEW_DIRECTORY [--version VERSION]' >&2; exit 1 ;;
    esac
done
for tool in curl jq sha256sum; do command -v "$tool" >/dev/null || { echo "Agent must prepare $tool" >&2; exit 1; }; done
[[ $(uname -m) == x86_64 && "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ && "$OUTPUT" == /* && ! -e "$OUTPUT" && ! -L "$OUTPUT" ]] || exit 1
base=$(jq -er .release_base_url "$PROJECT/release.config.json")
[[ "$base" == https://* && "$base" != *[\?\#\@]* ]] || exit 1
WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
for name in SHA256SUMS software.json matrix-release-linux-amd64; do
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 20 --max-time 300 \
        "$base/v$VERSION/$name" --output "$WORK/$name"
done
for name in software.json matrix-release-linux-amd64; do
    line=$(awk -v name="$name" '$2==name {print}' "$WORK/SHA256SUMS")
    [[ "$line" =~ ^[a-f0-9]{64}[[:space:]]+$name$ ]] || { echo "Missing or ambiguous checksum: $name" >&2; exit 1; }
    (cd "$WORK"; printf '%s\n' "$line" | sha256sum --quiet -c -)
done
jq -e --arg v "$VERSION" '.project=="matrix-admin" and .version==$v' "$WORK/software.json" >/dev/null
chmod 700 "$WORK/matrix-release-linux-amd64"
mkdir -p "$(dirname "$OUTPUT")"
"$WORK/matrix-release-linux-amd64" fetch --origin "$PROJECT/release.config.json" --descriptor "$WORK/software.json" --output "$OUTPUT"
printf 'Verified public runtime: %s\n' "$OUTPUT"

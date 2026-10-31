# Public software is a separately verified release. Never execute code from B2.
release_root() { (cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P); }
release_verify() {
    local root=$1
    "$(release_root)/backup/bin/matrix-release" verify --manifest "$root/release-manifest.json" --root "$root"
}
release_descriptor() {
    local root=$1
    release_verify "$root" || return 1
    "$root/backup/bin/matrix-release" describe --manifest "$root/release-manifest.json"
}
release_fetch() {
    local descriptor=$1 output=$2 trusted
    trusted=$(release_root)
    "$trusted/backup/bin/matrix-release" fetch --origin "$trusted/release.json" --descriptor "$descriptor" --output "$output"
}
# Install only the release inventory. Private config, identity and journal are
# outside this inventory. The same function is used on foundations/generations.
release_copy() (
    set -Eeuo pipefail
    local src=$1 dst=$2 name parent mode temporary=
    trap '[[ -z "$temporary" ]] || rm -f -- "$temporary"' EXIT
    release_verify "$src" || return 1
    [[ "$dst" == /* && "$dst" != / && ! -L "$dst" ]] || return 1
    [[ $(realpath -m -- "$dst") == "$dst" ]] || return 1
    # Check the entire destination inventory before replacing any file. Resolve
    # for comparison only: using the resolved parent would silently follow links.
    while IFS= read -r name; do
        parent=$dst
        [[ "$name" != */* ]] || parent="$dst/${name%/*}"
        [[ $(realpath -m -- "$parent") == "$parent" && ! -L "$dst/$name" ]] || return 1
        [[ ! -e "$dst/$name" || -f "$dst/$name" ]] || return 1
    done < <(jq -er '.files[].path, "release-manifest.json"' "$src/release-manifest.json")
    mkdir -p "$dst"
    [[ $(realpath -e "$dst") == "$dst" ]] || return 1
    while IFS=$'\t' read -r name mode; do
        parent=$dst
        [[ "$name" != */* ]] || parent="$dst/${name%/*}"
        mkdir -p "$parent"
        [[ $(realpath -e "$parent") == "$parent" && ! -L "$dst/$name" ]] || return 1
        temporary=$(mktemp "$parent/.matrix-release.XXXXXXXX")
        install -m "$mode" "$src/$name" "$temporary"
        mv -fT -- "$temporary" "$dst/$name"
        temporary=
    done < <(jq -er '.files[] | [.path,(if .executable then "700" else "600" end)] | @tsv' "$src/release-manifest.json")
    temporary=$(mktemp "$dst/.matrix-release.XXXXXXXX")
    install -m600 "$src/release-manifest.json" "$temporary"
    mv -fT -- "$temporary" "$dst/release-manifest.json"
    temporary=
    sync -f "$dst/release-manifest.json"
    release_verify "$dst"
)

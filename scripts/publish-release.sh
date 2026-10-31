#!/usr/bin/env bash
# CI-only GitHub glue. VPSes neither download nor execute this script.
set -euo pipefail
: "${RELEASE_TAG:?}" "${GH_REPO:?}" "${GH_TOKEN:?}"
assets=$(realpath "${1:?asset directory}")
expected_commit=$(git rev-parse HEAD)
remote_commit=$(gh api "repos/$GH_REPO/commits/$RELEASE_TAG" --jq .sha)
[[ "$remote_commit" == "$expected_commit" ]] || { echo 'Tag moved since tests'; exit 1; }
[[ $(find "$assets" -maxdepth 1 -type f | wc -l) -eq 4 ]] || { echo 'Unexpected build assets'; exit 1; }
(cd "$assets" && sha256sum --strict -c SHA256SUMS)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
tar -xzf "$assets/matrix-admin-linux-amd64.tar.gz" -C "$stage"
verified=$("$stage/matrix-admin" verify --from "$stage")
printf '%s\n' "$verified" > "$stage/verified.json"
[[ $(jq -r .commit "$stage/verified.json") == "$expected_commit" ]] || { echo 'Built commit differs'; exit 1; }
[[ "v$(jq -r .version "$stage/verified.json")" == "$RELEASE_TAG" ]] || { echo 'Built version differs'; exit 1; }
rm "$stage/verified.json"
if ! gh release view "$RELEASE_TAG" --json isDraft,assets > "$stage/release.json"; then
  gh release create "$RELEASE_TAG" --verify-tag --draft --title "Matrix Admin $RELEASE_TAG" --notes-file "$assets/notes.md"
  gh release view "$RELEASE_TAG" --json isDraft,assets > "$stage/release.json"
fi
# Never turn a published release back into a draft or overwrite an existing asset.
for name in $(jq -r '.assets[].name' "$stage/release.json"); do
  case "$name" in AGENT.md|SHA256SUMS|matrix-admin-linux-amd64.tar.gz) ;; *) echo 'Unexpected existing asset'; exit 1;; esac
done
mkdir "$stage/remote"
for name in AGENT.md SHA256SUMS matrix-admin-linux-amd64.tar.gz; do
  if jq -e --arg name "$name" '.assets | any(.name == $name)' "$stage/release.json" >/dev/null; then
    gh release download "$RELEASE_TAG" --pattern "$name" --dir "$stage/remote"
    cmp "$assets/$name" "$stage/remote/$name" || { echo 'Existing release asset differs'; exit 1; }
  else
    [[ $(jq -r .isDraft "$stage/release.json") == true ]] || { echo 'Published release is incomplete; use a new tag'; exit 1; }
    gh release upload "$RELEASE_TAG" "$assets/$name"
    gh release download "$RELEASE_TAG" --pattern "$name" --dir "$stage/remote"
    cmp "$assets/$name" "$stage/remote/$name"
  fi
done
if [[ $(jq -r .isDraft "$stage/release.json") == true ]]; then
  gh release edit "$RELEASE_TAG" --draft=false --title "Matrix Admin $RELEASE_TAG" --notes-file "$assets/notes.md"
fi
rm "$stage/release.json"

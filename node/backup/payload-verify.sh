# An explicit private-data schema prevents a snapshot from replacing public code.
# Empty directories are ignored; every regular file must have an owned data path.
payload_verify() (
    set -Eeuo pipefail
    local bundle=$1 mode=${2:-full} root file rel line wanted actual tmp
    root="$bundle/payload"
    [[ -d "$root" && -s "$bundle/manifest.sha256" && ! -L "$root" ]] || exit 1
    [[ -z $(find "$root" ! -type f ! -type d -print -quit) ]] || { echo 'Unsupported object in private payload' >&2; exit 1; }
    tmp=$(mktemp)
    trap 'rm -f -- "$tmp"' EXIT
    while IFS= read -r -d '' file; do
        rel=${file#"$root/"}
        [[ "$rel" =~ ^[a-zA-Z0-9_./-]+$ && "$rel" != *..* ]] || exit 1
        case "$rel" in
            database/*) [[ "${rel#database/}" != */* ]] || exit 1 ;;
            config/appservices/*) [[ "${rel#config/appservices/}" != */* ]] || exit 1 ;;
            config/docker-compose.yml|config/registration-token|config/.env|config/s3/media.env|telegram/config/docker-compose.yml|telegram/data/telegram.db|telegram/data/config.yaml|telegram/data/registration.yaml|telegram/metadata.json|nginx/matrix.conf|nginx/00-matrix-common.conf|control/source-local.json) ;;
            *) printf 'Unclassified private payload path: %s\n' "$rel" >&2; exit 1 ;;
        esac
        actual=$(sha256sum -- "$file" | cut -d' ' -f1)
        printf '%s  ./%s\n' "$actual" "$rel" >>"$tmp"
        if [[ "$mode" == partial ]]; then
            line=$(awk -v wanted="./$rel" 'substr($0,67)==wanted {print}' "$bundle/manifest.sha256")
            [[ "$line" == "$actual  ./$rel" ]] || { echo 'Private configuration hash differs' >&2; exit 1; }
        fi
    done < <(find "$root" -type f -print0 | LC_ALL=C sort -z)
    case "$mode" in
        full) cmp -s "$tmp" "$bundle/manifest.sha256" || { echo 'Private payload inventory/hash differs' >&2; exit 1; } ;;
        partial) [[ -s "$tmp" ]] ;;
        *) exit 1 ;;
    esac
)

# Recovery must use the configured registry image, not an arbitrary local alias.
image_reference() {
    local container=$1 expected=$2 ref registry
    [[ "$expected" =~ ^sha256:[a-f0-9]{64}$ ]] || return 1
    ref=$("$DOCKER_BIN" inspect --format '{{json .}}' "$container" |
        jq -er --arg id "$expected" 'select(.Image==$id) | .Config.Image | select(type=="string")') || {
        printf 'ERROR: Container image changed while preparing its recovery reference\n' >&2; return 1;
    }
    [[ "$ref" =~ ^[a-zA-Z0-9][a-zA-Z0-9._:/-]+@sha256:[a-f0-9]{64}$ ]] || {
        printf 'ERROR: Configure the application image with an immutable registry digest before backup\n' >&2; return 1;
    }
    registry=${ref%%/*}
    [[ "$registry" != "$ref" && ( "$registry" == *.* || "$registry" == *:* ) ]] || {
        printf 'ERROR: Recovery image must include an explicit registry hostname\n' >&2; return 1;
    }
    [[ $("$DOCKER_BIN" image inspect --format '{{.Id}}' "$ref") == "$expected" ]] || {
        printf 'ERROR: Configured registry digest resolves to a different image\n' >&2; return 1;
    }
    printf '%s\n' "$ref"
}

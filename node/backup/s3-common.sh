# Private data/configuration transport; public executables come from project releases.
set -Eeuo pipefail
umask 077
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
note() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
load_s3() {
    [[ -f "$1" && $(stat -c %a "$1") =~ ^[046]00$ ]] || fail 'Use a private (0600) S3 configuration file'
    CONFIG=$(realpath -e -- "$1")
    # Trusted administrator configuration, never a file fetched from S3.
    source "$CONFIG"
    S3_REGION=${S3_REGION:-auto}
    S3_ENDPOINT=${S3_ENDPOINT:-}
    S3_ENDPOINT=${S3_ENDPOINT%/}
    [[ ${CLUSTER_ID:-} =~ ^[a-zA-Z0-9_-][a-zA-Z0-9_.-]{0,99}$ && "$CLUSTER_ID" != *..* ]] || fail 'Configure the intended CLUSTER_ID in the R2 input'
    RECOVERY_PREFIX="cluster/$CLUSTER_ID/recovery/v2"
    [[ "$S3_REGION" == auto && ${S3_BUCKET:-} =~ ^[a-zA-Z0-9-]+$ ]] || fail 'Invalid R2 region/bucket'
    [[ "$S3_ENDPOINT" =~ ^https://[a-zA-Z0-9.-]+\.r2\.cloudflarestorage\.com$ ]] || fail 'Recovery bootstrap requires an authenticated R2 endpoint'
    [[ ${AWS_ACCESS_KEY_ID:-} =~ ^[a-zA-Z0-9_-]+$ && ${AWS_SECRET_ACCESS_KEY:-} =~ ^[a-zA-Z0-9/+=_-]+$ ]] || fail 'Invalid or missing R2 credentials'
    [[ -z ${AWS_SESSION_TOKEN:-} ]] || fail 'Temporary session credentials are not supported by this protocol'
    AUTH=$(mktemp)
    chmod 600 "$AUTH"
    printf 'aws-sigv4 = "aws:amz:%s:s3"\nuser = "%s:%s"\n' "$S3_REGION" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >"$AUTH"
}
s3() {
    local method=$1 key=$2 file=$3 condition=${4:-} code
    local -a conditional=()
    [[ -z "$condition" ]] || conditional=(--header "$condition")
    [[ "$key" =~ ^[a-zA-Z0-9._/-]+$ && "$key" != *..* ]] || fail 'Invalid S3 object key'
    if [[ "$method" == HEAD ]]; then
        code=$(curl --config "$AUTH" --silent --show-error --proto '=https' --connect-timeout 20 --max-time 60 \
            --retry 3 --head --output "$file" --write-out '%{http_code}' "$S3_ENDPOINT/$S3_BUCKET/$key") || return 1
    elif [[ "$method" == GET ]]; then
        code=$(curl --config "$AUTH" --silent --show-error --proto '=https' --connect-timeout 20 --max-time 1800 \
            --max-filesize 33554432 --retry 3 --retry-delay 2 --dump-header "$WORK/headers" --output "$file" --write-out '%{http_code}' "$S3_ENDPOINT/$S3_BUCKET/$key") || return 1
    else
        code=$(curl --config "$AUTH" --silent --show-error --proto '=https' --connect-timeout 20 --max-time 1800 \
            --retry 3 --retry-delay 2 --header "x-amz-content-sha256: $(hash_file "$file")" \
            "${conditional[@]}" --request PUT --upload-file "$file" --output /dev/null --write-out '%{http_code}' \
            "$S3_ENDPOINT/$S3_BUCKET/$key") || return 1
    fi
    [[ "$code" == 200 || "$code" == 204 ]] && return 0
    [[ "$code" == 404 ]] && return 44
    [[ "$code" == 412 ]] && return 42
    printf 'S3 %s failed (HTTP %s), object: %s\n' "$method" "$code" "$key" >&2
    return 1
}
hash_file() { sha256sum -- "$1" | cut -d ' ' -f1; }
verify_hash() {
    [[ "$2" =~ ^[a-f0-9]{64}$ ]] || fail 'Invalid expected SHA256'
    [[ $(hash_file "$1") == "$2" ]] || fail "SHA256 mismatch: $(basename "$1")"
}
object_etag() { awk 'tolower($1)=="etag:" {gsub("\r", "", $2); print $2}' "$WORK/headers" | tail -1; }
put_verified() {
    local key=$1 file=$2 status=0
    s3 PUT "$key" "$file" 'If-None-Match: *' || status=$?
    [[ "$status" == 0 || "$status" == 42 ]] || fail "Immutable R2 upload failed: $key"
    s3 GET "$key" "$WORK/compare" || fail "R2 read-back failed: $key"
    cmp -s -- "$file" "$WORK/compare" || fail "Immutable R2 object differs: $key"
}
capture_latest() {
    local status=0
    s3 GET "$RECOVERY_PREFIX/latest.json" "$WORK/previous-latest.json" || status=$?
    if (( status == 44 )); then LATEST_CONDITION='If-None-Match: *'
    elif (( status == 0 )); then
        local etag; etag=$(object_etag)
        [[ "$etag" =~ ^\"[a-zA-Z0-9-]+\"$ ]] || fail 'Missing/invalid R2 latest ETag'
        LATEST_CONDITION="If-Match: $etag"
    else fail 'Cannot inspect the current R2 recovery index'; fi
}
commit_latest() {
    s3 PUT "$RECOVERY_PREFIX/latest.json" "$WORK/latest.json" "$LATEST_CONDITION" || fail 'R2 latest changed or upload failed; previous success is not advanced'
    s3 GET "$RECOVERY_PREFIX/latest.json" "$WORK/compare" || fail 'Cannot confirm R2 index commit'
    cmp -s "$WORK/latest.json" "$WORK/compare" || fail 'R2 recovery index changed after commit'
}

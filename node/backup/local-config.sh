# Trusted local host settings, supplied by the restoring Agent, never by S3.
host_defaults() {
    RESTORE_ROOT=/opt/matrix
    WORK_ROOT=$RESTORE_ROOT/generations
    LOCK_FILE=$RESTORE_ROOT/agent/recovery.lock
    DOCKER_BIN=docker
    COMPOSE_BIN=
    SQLITE_BIN=sqlite3
    NGINX_BIN=nginx
    NGINX_MAIN_CONF=/etc/nginx/nginx.conf
    NGINX_HTTP_CONF_DIR=/etc/nginx/conf.d
    NGINX_SERVICE=nginx
    SYSTEMCTL_BIN=systemctl
    SYSTEMD_UNIT_DIR=/etc/systemd/system
}
host_load() {
    [[ -f "$1" && $(stat -c %a "$1") =~ ^[046]00$ ]] || { printf 'Local config must be private (0600)\n' >&2; return 1; }
    # Administrator-maintained shell configuration, not a remote executable.
    source "$1"
    [[ ${LOCAL_CONFIG_SCHEMA:-} == 1 ]] || { printf 'Unsupported local config schema\n' >&2; return 1; }
    host_validate
}
host_validate() {
    local name value
    for name in RESTORE_ROOT WORK_ROOT LOCK_FILE NGINX_MAIN_CONF NGINX_HTTP_CONF_DIR SYSTEMD_UNIT_DIR; do
        value=${!name}
        [[ "$value" == /* && "$value" != / && "$value" != *[!a-zA-Z0-9_./-]* ]] || { printf 'Invalid local path: %s\n' "$name" >&2; return 1; }
        printf -v "$name" '%s' "$(realpath -m -- "$value")"
        [[ ${!name} != / ]] || { printf 'Local path resolves to root: %s\n' "$name" >&2; return 1; }
    done
    for name in DOCKER_BIN COMPOSE_BIN SQLITE_BIN NGINX_BIN SYSTEMCTL_BIN; do
        value=${!name}
        [[ "$name" == COMPOSE_BIN && -z "$value" ]] && continue
        [[ -n "$value" && "$value" != *[!a-zA-Z0-9_./-]* ]] || { printf 'Use an executable path/name, without arguments: %s\n' "$name" >&2; return 1; }
    done
    [[ "$NGINX_SERVICE" =~ ^[a-zA-Z0-9_.@-]+$ && "$NGINX_SERVICE" != -* ]] || { printf 'Invalid nginx service name\n' >&2; return 1; }
}
host_emit() {
    local name
    printf '\nLOCAL_CONFIG_SCHEMA=1\n'
    for name in RESTORE_ROOT WORK_ROOT LOCK_FILE DOCKER_BIN COMPOSE_BIN SQLITE_BIN NGINX_BIN NGINX_MAIN_CONF NGINX_HTTP_CONF_DIR NGINX_SERVICE SYSTEMCTL_BIN SYSTEMD_UNIT_DIR; do
        printf '%s=%q\n' "$name" "${!name}"
    done
}
docker() { command "$DOCKER_BIN" "$@"; }
compose() {
    if [[ -n "$COMPOSE_BIN" ]]; then command "$COMPOSE_BIN" "$@"; else docker compose "$@"; fi
}
nginx_cmd() { command "$NGINX_BIN" -c "$NGINX_MAIN_CONF" "$@"; }
systemctl_cmd() { command "$SYSTEMCTL_BIN" "$@"; }

# The only existing tree a fresh foundation may adopt is its bootstrap envelope.
# Observations/downloads are not deployments. Unknown data, symlinks, an existing
# deploy tree or a recovery generation are never overwritten or classified empty.
host_bootstrap_target() {
    local root=$1 entry
    [[ "$root" == /* && "$root" != / && "$root" != *[!a-zA-Z0-9_./-]* && $(realpath -m -- "$root") == "$root" && ! -L "$root" ]] || return 1
    [[ -e "$root" ]] || return 0
    [[ -d "$root" && -f "$root/config/s3.env" && ! -L "$root/config/s3.env" && $(stat -c %a "$root/config/s3.env") =~ ^[046]00$ ]] || return 1
    [[ -z $(find "$root" -type l -print -quit) ]] || return 1
    while IFS= read -r -d '' entry; do
        case "${entry##*/}" in config|agent|generations) [[ -d "$entry" ]] || return 1 ;; *) return 1 ;; esac
    done < <(find "$root" -mindepth 1 -maxdepth 1 -print0)
    [[ -z $(find "$root/config" -mindepth 1 -maxdepth 1 ! -name s3.env -print -quit) ]] || return 1
    [[ ! -d "$root/generations" || -z $(find "$root/generations" -mindepth 1 -print -quit) ]] || return 1
}

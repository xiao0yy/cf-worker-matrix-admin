#!/usr/bin/env bash
# Public release entry. Dependencies are prepared by the Agent, never installed here.
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")" && pwd -P)
source "$BASE/s3-common.sh"
source "$BASE/local-config.sh"
source "$BASE/release-common.sh"
RECOVER_ARGS=("$@")
source "$BASE/recover-main.sh"

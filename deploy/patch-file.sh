#!/usr/bin/env bash
# Render only: never mutate the source. Refuse unknown or partially patched files.
set -Eeuo pipefail
source_file=$1
kind=$2
BASE=$(cd -- "$(dirname -- "$0")" && pwd)
case "$kind" in
 producer) needle='# Preserve host settings only as a source reference';insert="$BASE/producer-insert.txt";mode=before ;;
 standby) needle='bash "$CONTROL/handover-worker.sh" --install';insert="$BASE/standby-insert.txt";mode=after ;;
 *) exit 1 ;;
esac
if awk '/MATRIX_MAILBOX_V1/ {found=1} END {exit !found}' "$source_file"; then
    original=$(cat "$source_file")
    block=$(cat "$insert")
    [[ "$original" == *"$block"* ]] || { echo 'Partial mailbox patch; Agent must review it' >&2;exit 1; }
    cat "$source_file"
    exit 0
fi
count=$(awk -v needle="$needle" 'index($0,needle) {n++} END {print n+0}' "$source_file")
[[ "$count" == 1 ]] || { echo "Unsupported source layout ($kind); Agent must review before integrating" >&2;exit 1; }
awk -v needle="$needle" -v insert="$insert" -v mode="$mode" '
function addition(){while((getline line < insert)>0)print line;close(insert)}
index($0,needle) {if(mode=="before")addition();print;if(mode=="after")addition();next}
{print}' "$source_file"

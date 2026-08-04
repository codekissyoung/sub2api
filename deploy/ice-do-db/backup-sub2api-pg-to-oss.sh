#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

OSSUTIL="${OSSUTIL:-/home/iec/ossutil}"
OSS_BUCKET="${OSS_BUCKET:-oss://icodeeasy/backup/sub2api-pg}"
RETENTION_DAYS="${RETENTION_DAYS:-7}"
PG_DB="${PG_DB:-sub2api}"

timestamp=$(date -u +%Y%m%d-%H%M%S)
base_name="${PG_DB}-${timestamp}"
tmp_dir=$(mktemp -d /tmp/sub2api-pg-backup.XXXXXX)
dump_file="${tmp_dir}/${base_name}.pgdump"
manifest_file="${tmp_dir}/${base_name}.manifest.txt"
sha256_file="${tmp_dir}/${base_name}.sha256"
uploaded_objects=()

log() {
  printf '[%s] %s\n' "$(date -u '+%Y-%m-%d %H:%M:%S UTC')" "$*"
}

cleanup() {
  exit_code=$?
  if ((exit_code != 0)); then
    for remote_object in "${uploaded_objects[@]:-}"; do
      [[ -n "$remote_object" ]] || continue
      "$OSSUTIL" rm "$remote_object" --force >/dev/null 2>&1 || true
    done
  fi
  rm -rf "$tmp_dir"
  exit "$exit_code"
}
trap cleanup EXIT

for command_name in pg_dump pg_restore sha256sum sudo; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "missing command: $command_name" >&2
    exit 1
  }
done
[[ -x "$OSSUTIL" ]] || { echo "missing ossutil: $OSSUTIL" >&2; exit 1; }

verify_remote_size() {
  remote_path=$1
  local_path=$2
  local_size=$(stat -c '%s' "$local_path")
  remote_size=$(
    "$OSSUTIL" stat "$remote_path" | awk -F: '
      $1 ~ /^Content-Length[[:space:]]*$/ {
        value = $2
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
        print value
      }
    '
  )
  [[ -n "$remote_size" && "$remote_size" == "$local_size" ]]
}

upload_with_verify() {
  local_path=$1
  remote_path=$2
  "$OSSUTIL" cp "$local_path" "$remote_path" --force
  uploaded_objects+=("$remote_path")
  verify_remote_size "$remote_path" "$local_path"
}

log "dumping PostgreSQL database ${PG_DB}"
sudo -n -u postgres pg_dump \
  --dbname="$PG_DB" \
  --format=custom \
  --compress=6 \
  --create >"$dump_file"
pg_restore -l "$dump_file" >/dev/null

{
  echo "timestamp_utc=$timestamp"
  echo "hostname=$(hostname)"
  echo "database=$PG_DB"
  echo "dump_file=$(basename "$dump_file")"
  echo "dump_size_bytes=$(stat -c '%s' "$dump_file")"
  echo "restore_command=pg_restore --clean --if-exists --create -d postgres $(basename "$dump_file")"
} >"$manifest_file"

(
  cd "$tmp_dir"
  sha256sum "$(basename "$dump_file")" "$(basename "$manifest_file")"
) >"$sha256_file"

daily_prefix="${OSS_BUCKET}/daily/${base_name}"
upload_with_verify "$dump_file" "${daily_prefix}.pgdump"
upload_with_verify "$manifest_file" "${daily_prefix}.manifest.txt"
upload_with_verify "$sha256_file" "${daily_prefix}.sha256"
upload_with_verify "$dump_file" "${OSS_BUCKET}/latest.pgdump"
upload_with_verify "$manifest_file" "${OSS_BUCKET}/latest.manifest.txt"
upload_with_verify "$sha256_file" "${OSS_BUCKET}/latest.sha256"

cutoff_date=$(date -u -d "${RETENTION_DAYS} days ago" +%Y%m%d)
while read -r remote_object; do
  [[ -n "$remote_object" ]] || continue
  object_name=$(basename "$remote_object")
  file_date=""
  if [[ "$object_name" == "${PG_DB}-"* ]]; then
    file_date=${object_name:${#PG_DB}+1:8}
  fi
  if [[ "$file_date" =~ ^[0-9]{8}$ ]] && ((10#$file_date < 10#$cutoff_date)); then
    "$OSSUTIL" rm "$remote_object" --force
  fi
done < <("$OSSUTIL" ls "${OSS_BUCKET}/daily/" 2>/dev/null | awk '/^20[0-9]{2}-/ {print $NF}')

log "backup complete: ${daily_prefix}.pgdump"

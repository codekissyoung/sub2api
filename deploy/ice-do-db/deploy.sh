#!/usr/bin/env bash
# deploy.sh — Build Sub2API once, deploy to the ice production hosts.
#
# Hard-codes the ONE thing that bit us on 2026-08-13: the Go binary must be
# built with `-tags embed`, otherwise the frontend is not embedded and every
# page route returns 404 while APIs keep working. Do not hand-run `go build`
# or `make build` for production — use this script.
#
# The build (frontend lint/build + backend tests + backend build) runs ONCE;
# the same binary is then pushed to every host in turn, so each extra host
# costs ~30s instead of a full rebuild.
#
# Usage:   deploy/ice-do-db/deploy.sh [host ...]
#          default hosts: ice-do-db ice-do-web-2
# Env:     SKIP_TESTS=1  SKIP_BACKUP=1 (skip the one-time DB backup)
#          ALLOW_DIRTY=1
set -euo pipefail

DEPLOY_DIR="/home/iec/deploy"
PUBLIC_URL="https://sub2api.ieasycode.cc"
PAGE_PROBE="/admin/accounts"
LEDGER_HOST="ice-do-db"  # hosts the shared release ledger and the DB backup job

# host -> listener base URL (VPC address, port 8320)
listen_url() {
  case "$1" in
    ice-do-db)    echo "http://10.124.0.3:8320" ;;
    ice-do-web-2) echo "http://10.124.0.5:8320" ;;
    *) return 1 ;;
  esac
}

if [[ $# -gt 0 ]]; then
  HOSTS=("$@")
else
  HOSTS=(ice-do-db ice-do-web-2)
fi
for h in "${HOSTS[@]}"; do
  listen_url "$h" >/dev/null || { echo "unknown host: $h (extend listen_url in $0)" >&2; exit 1; }
done

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

echo "==> 0/6 preflight"
if [[ "${ALLOW_DIRTY:-0}" != "1" ]]; then
  if [[ -n "$(git status --porcelain)" ]]; then
    echo "working tree is dirty; commit first (or ALLOW_DIRTY=1)" >&2
    exit 1
  fi
fi
COMMIT="$(git rev-parse --short=9 HEAD)"
VERSION="$(tr -d '[:space:]' < backend/cmd/server/VERSION)"
TS="$(date -u +%Y%m%dT%H%M%SZ)"
REMOTE_BIN="sub2api.${TS}-${COMMIT}"
echo "    commit=${COMMIT} version=${VERSION} hosts=${HOSTS[*]}"

echo "==> 1/6 frontend build (corepack pnpm lint + vue-tsc + vite)"
corepack pnpm --dir frontend run lint:check
# `build` already runs `vue-tsc -b` before vite, so no separate typecheck step.
corepack pnpm --dir frontend run build   # vite outDir -> backend/internal/web/dist

echo "==> 2/6 backend tests"
if [[ "${SKIP_TESTS:-0}" != "1" ]]; then
  (cd backend && go test ./...)
else
  echo "    skipped (SKIP_TESTS=1)"
fi

echo "==> 3/6 backend build (MANDATORY -tags embed)"
(cd backend && CGO_ENABLED=0 go build -tags embed -ldflags="-s -w -X main.Version=${VERSION}" -trimpath -o bin/server ./cmd/server)

echo "==> 4/6 verify embedded frontend is really in the binary"
# embed_on.go (tag `embed`) embeds dist and contains this nonce placeholder;
# the !embed stub does not. No marker => wrong build tags => page 404s.
if ! grep -aq "__CSP_NONCE_VALUE__" backend/bin/server; then
  echo "FATAL: binary lacks embedded frontend (missing -tags embed?)" >&2
  exit 1
fi
echo "    embed marker found"

echo "==> 5/6 pre-deploy database backup (once, shared DB)"
backed_up=0
if [[ "${SKIP_BACKUP:-0}" != "1" ]]; then
  for h in "${HOSTS[@]}"; do
    if [[ "$h" == "$LEDGER_HOST" ]]; then
      ssh -o BatchMode=yes "$LEDGER_HOST" \
        'flock -n /tmp/backup-sub2api-pg.lock /home/iec/deploy/bin/backup-sub2api-pg-to-oss' | tail -1
      backed_up=1
    fi
  done
fi
[[ "$backed_up" == "1" ]] || echo "    skipped (SKIP_BACKUP=1 or ${LEDGER_HOST} not in host list)"

echo "==> 6/6 per host: upload + flip + restart + verify"
for h in "${HOSTS[@]}"; do
  listen="$(listen_url "$h")"
  echo "--- ${h} (${listen})"
  scp -o BatchMode=yes backend/bin/server "${h}:${DEPLOY_DIR}/bin/${REMOTE_BIN}"
  ssh -o BatchMode=yes "$h" "set -e
    chmod +x '${DEPLOY_DIR}/bin/${REMOTE_BIN}'
    grep -aq '__CSP_NONCE_VALUE__' '${DEPLOY_DIR}/bin/${REMOTE_BIN}'
    ln -sfn '${DEPLOY_DIR}/bin/${REMOTE_BIN}' '${DEPLOY_DIR}/bin/sub2api'
    sudo -n systemctl restart sub2api
    sleep 3
    systemctl is-active sub2api
    curl -fsS --max-time 10 -o /dev/null '${listen}/health'
    code=\$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' '${listen}${PAGE_PROBE}')
    [ \"\$code\" = 200 ] || { echo \"FATAL: ${PAGE_PROBE} -> \$code (embed missing?)\" >&2; exit 1; }
    echo \"    origin ok: /health 200, ${PAGE_PROBE} 200\""
  ssh -o BatchMode=yes "$LEDGER_HOST" "cat >> '${DEPLOY_DIR}/release-ledger/events.jsonl'" <<EOF
{"ts":"$(date -Iseconds)","action":"deploy","host":"${h}","version":"${VERSION}","commit":"${COMMIT}","binary":"${REMOTE_BIN}","via":"deploy/ice-do-db/deploy.sh","operator":"${USER:-unknown}"}
EOF
done

public_code="$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' "${PUBLIC_URL}${PAGE_PROBE}")"
[[ "$public_code" == "200" ]] || { echo "FATAL: public ${PAGE_PROBE} -> ${public_code}" >&2; exit 1; }
echo "    public ok: ${PUBLIC_URL}${PAGE_PROBE} 200"

echo "==> deployed ${VERSION} (${COMMIT}) as ${REMOTE_BIN} to: ${HOSTS[*]}"

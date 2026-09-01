#!/usr/bin/env bash
# deploy.sh — Build and deploy Sub2API to the ice-do-db production host.
#
# Hard-codes the ONE thing that bit us on 2026-08-13: the Go binary must be
# built with `-tags embed`, otherwise the frontend is not embedded and every
# page route returns 404 while APIs keep working. Do not hand-run `go build`
# or `make build` for production — use this script.
#
# Usage:   deploy/ice-do-db/deploy.sh        (run once per host: ice-do-db, then ice-do-web-2)
# Env:     REMOTE=ice-do-db|ice-do-web-2  SKIP_TESTS=1  SKIP_BACKUP=1  ALLOW_DIRTY=1
#          LISTEN=http://<vpc-ip>:8320 (override for other hosts)
set -euo pipefail

REMOTE="${REMOTE:-ice-do-db}"
DEPLOY_DIR="/home/iec/deploy"
PUBLIC_URL="https://sub2api.ieasycode.cc"
PAGE_PROBE="/admin/accounts"

case "$REMOTE" in
  ice-do-db)    DEFAULT_LISTEN="http://10.124.0.3:8320" ;;
  ice-do-web-2) DEFAULT_LISTEN="http://10.124.0.5:8320" ;;
  *)            DEFAULT_LISTEN="" ;;
esac
LISTEN="${LISTEN:-$DEFAULT_LISTEN}"
if [[ -z "$LISTEN" ]]; then
  echo "unknown REMOTE=$REMOTE; set LISTEN=http://<vpc-ip>:8320 explicitly" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

echo "==> 0/7 preflight"
if [[ "${ALLOW_DIRTY:-0}" != "1" ]]; then
  if [[ -n "$(git status --porcelain)" ]]; then
    echo "working tree is dirty; commit first (or ALLOW_DIRTY=1)" >&2
    exit 1
  fi
fi
COMMIT="$(git rev-parse --short=9 HEAD)"
VERSION="$(tr -d '[:space:]' < backend/cmd/server/VERSION)"
echo "    commit=${COMMIT} version=${VERSION}"

echo "==> 1/7 frontend build (corepack pnpm)"
corepack pnpm --dir frontend run lint:check
corepack pnpm --dir frontend run typecheck
corepack pnpm --dir frontend run build   # vite outDir -> backend/internal/web/dist

echo "==> 2/7 backend tests"
if [[ "${SKIP_TESTS:-0}" != "1" ]]; then
  (cd backend && go test ./...)
else
  echo "    skipped (SKIP_TESTS=1)"
fi

echo "==> 3/7 backend build (MANDATORY -tags embed)"
(cd backend && CGO_ENABLED=0 go build -tags embed -ldflags="-s -w -X main.Version=${VERSION}" -trimpath -o bin/server ./cmd/server)

echo "==> 4/7 verify embedded frontend is really in the binary"
# embed_on.go (tag `embed`) embeds dist and contains this nonce placeholder;
# the !embed stub does not. No marker => wrong build tags => page 404s.
if ! grep -aq "__CSP_NONCE_VALUE__" backend/bin/server; then
  echo "FATAL: binary lacks embedded frontend (missing -tags embed?)" >&2
  exit 1
fi
echo "    embed marker found"

echo "==> 5/7 pre-deploy database backup"
if [[ "${SKIP_BACKUP:-0}" != "1" ]]; then
  ssh -o BatchMode=yes "$REMOTE" \
    'flock -n /tmp/backup-sub2api-pg.lock /home/iec/deploy/bin/backup-sub2api-pg-to-oss' | tail -1
else
  echo "    skipped (SKIP_BACKUP=1; shared DB already backed up this release)"
fi

echo "==> 6/7 upload + flip + restart"
TS="$(date -u +%Y%m%dT%H%M%SZ)"
REMOTE_BIN="sub2api.${TS}-${COMMIT}"
scp -o BatchMode=yes backend/bin/server "${REMOTE}:${DEPLOY_DIR}/bin/${REMOTE_BIN}"
ssh -o BatchMode=yes "$REMOTE" "set -e
  chmod +x '${DEPLOY_DIR}/bin/${REMOTE_BIN}'
  ln -sfn '${DEPLOY_DIR}/bin/${REMOTE_BIN}' '${DEPLOY_DIR}/bin/sub2api'
  sudo -n systemctl restart sub2api
  sleep 3
  systemctl is-active sub2api"

echo "==> 7/7 verify: health AND a page route (catch non-embed builds)"
ssh -o BatchMode=yes "$REMOTE" "set -e
  curl -fsS --max-time 10 -o /dev/null '${LISTEN}/health'
  code=\$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' '${LISTEN}${PAGE_PROBE}')
  [ \"\$code\" = 200 ] || { echo \"FATAL: ${PAGE_PROBE} -> \$code (embed missing?)\" >&2; exit 1; }
  echo \"    origin ok: /health 200, ${PAGE_PROBE} 200\""
public_code="$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' "${PUBLIC_URL}${PAGE_PROBE}")"
[[ "$public_code" == "200" ]] || { echo "FATAL: public ${PAGE_PROBE} -> ${public_code}" >&2; exit 1; }
echo "    public ok: ${PUBLIC_URL}${PAGE_PROBE} 200"

ssh -o BatchMode=yes "$REMOTE" "cat >> '${DEPLOY_DIR}/release-ledger/events.jsonl'" <<EOF
{"ts":"$(date -Iseconds)","action":"deploy","version":"${VERSION}","commit":"${COMMIT}","binary":"${REMOTE_BIN}","via":"deploy/ice-do-db/deploy.sh","operator":"${USER:-unknown}"}
EOF

echo "==> deployed ${VERSION} (${COMMIT}) as ${REMOTE_BIN}"

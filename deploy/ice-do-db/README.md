# ICE production deployment

This directory records the production runtime convention for the Sub2API
account-pool instances. The pool now runs on TWO hosts behind the same
public hostname — `ice-do-db` (10.124.0.3:8320) and `ice-do-web-2`
(10.124.0.5:8320) — sharing the PostgreSQL/Redis on `ice-do-db`.
(`ice-do-web-1` is decommissioned; do not deploy to it.)

Deploy builds ONCE and pushes the same binary to both hosts in turn:

```bash
deploy/ice-do-db/deploy.sh              # build once, deploy ice-do-db + ice-do-web-2
deploy/ice-do-db/deploy.sh ice-do-db    # single host only
```

The shared database is backed up once (on `ice-do-db`) before any host is
touched; `SKIP_BACKUP=1` skips it. The second host costs ~30s (upload +
flip + restart + verify), not a rebuild.

## Runtime layout

```text
/home/iec/sub2api/                         source checkout (`ice` branch)
/home/iec/deploy/bin/sub2api               active binary symlink
/home/iec/deploy/bin/sub2api.<version>     immutable versioned binaries
/home/iec/deploy/etc/sub2api.yaml          live configuration (0600; never commit)
/home/iec/deploy/sub2api/                  setup lock and local application data
/home/iec/deploy/log/sub2api.log           application log
/home/iec/deploy/log/sub2api-systemd.log   early startup/systemd output
/home/iec/deploy/bin/backup-sub2api-pg-to-oss  PostgreSQL OSS backup job
```

The application uses the existing PostgreSQL server through a dedicated
`sub2api` role/database and a dedicated localhost Redis instance managed as
`redis-server@sub2api.service`. Database and Redis credentials stay only in
the live mode-0600 configuration.

## Network boundary

- Application listener: the `ice-do-db` DO VPC address on port `8320`.
- API origins reach it only through the DO VPC.
- The management hostname terminates TLS at the live `ice-do-db` Nginx.
- Port `8320` must not be opened to the public Internet.

## Source build

Always deploy with `deploy/ice-do-db/deploy.sh` — it runs the full flow
(frontend build, tests, backend build, backup, install, restart, health AND
page-route verification, ledger). The Go binary MUST be built with
`-tags embed`; without it the frontend is not embedded and every page route
returns 404 while APIs keep working (incident 2026-08-13). The script refuses
to ship a binary without the embed marker, so do not hand-run `go build` or
`make build` for production.

Manual build steps (only for reference — the script runs these):

```bash
corepack enable
corepack prepare pnpm@9 --activate
pnpm --dir frontend install --frozen-lockfile
pnpm --dir frontend run lint:check
pnpm --dir frontend run typecheck
pnpm --dir frontend run build

cd backend
GOTOOLCHAIN=auto go test ./...
CGO_ENABLED=0 GOTOOLCHAIN=auto go build -tags embed -trimpath ./cmd/server
```

Production artifacts must come from a clean, pushed `ice` commit. Install the
binary as `sub2api.<timestamp>-<short-commit>`, atomically flip the `sub2api`
symlink, restart the unit, verify `/health` and the systemd MainPID executable,
and append the result to the shared release ledger. Keep at least five previous
binaries for rollback.

Database migrations are forward-only. Back up the `sub2api` database before
every upgrade that contains migrations. Production also runs
`backup-sub2api-pg-to-oss.sh` four times per day under `flock`; it uses local
PostgreSQL peer authentication, writes no database password, verifies uploaded
object sizes, and retains seven days of database archives.

The reviewed Nginx vhost is `sub2api.nginx.conf`. It uses the existing
`*.ieasycode.cc` Cloudflare Origin CA certificate and proxies only to the VPC
listener; keep Cloudflare proxying enabled for the public management hostname.

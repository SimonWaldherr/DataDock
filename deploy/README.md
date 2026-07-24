# Deploying DataDock

DataDock is a single, self-contained, pure-Go binary (the UI templates and
static assets are embedded), so deployment is mostly "run one executable behind
a TLS-terminating reverse proxy." This directory holds ready-to-adapt recipes.

## The production topology

DataDock's own listener speaks **plain HTTP only**. For anything reachable
beyond `localhost`, terminate TLS at a reverse proxy (Caddy, nginx, Traefik)
and keep DataDock bound to loopback:

```
Browser ──HTTPS──> Caddy/nginx (:443) ──HTTP──> DataDock (127.0.0.1:8080)
```

Run DataDock with `-behind-tls-proxy` so it marks the session cookie `Secure`
even though its own connection is plain HTTP.

On **first run**, open the site and create the first Admin account (or choose
the single-user no-login mode) — see the *Admin Settings* section of the top-level
README. Until an account exists, admin routes return `428 Precondition Required`.

## 1. Docker / Compose

```bash
# Build and run with a persistent named volume:
docker compose -f deploy/docker-compose.yml up -d --build

# Or plain docker:
docker build -t datadock:latest --build-arg VERSION="$(git describe --tags --always --dirty)" .
docker run -d --name datadock -p 127.0.0.1:8080:8080 \
  -v datadock-data:/data datadock:latest
```

The image is `distroless/static` running as the non-root uid `65532`, with a
built-in `HEALTHCHECK` that probes `/healthz` via the binary's own
`-healthcheck` flag (no shell/wget needed). Persist `/data` with a volume.

## 2. systemd (native install)

```bash
sudo useradd --system --home /var/lib/datadock --shell /usr/sbin/nologin datadock
sudo install -m 0755 datadock /usr/local/bin/datadock
sudo cp deploy/datadock.service /etc/systemd/system/datadock.service
sudo systemctl daemon-reload
sudo systemctl enable --now datadock
```

The unit binds loopback, enables `disk` storage mode, and is hardened
(`ProtectSystem=strict`, no capabilities, `StateDirectory=datadock`). Set
`DATADOCK_ENCRYPTION_KEY` in the unit to encrypt stored secrets at rest.

## 3. Reverse proxy + TLS

See [`Caddyfile`](Caddyfile) for an automatic-HTTPS Caddy config (and an nginx
server block for reference). Point it at `127.0.0.1:8080`.

## Health & readiness

- `GET /healthz` — cheap, unauthenticated liveness probe (always `200 ok`).
- `GET /version` — unauthenticated build identity (`{"version":"..."}`), for
  detecting version drift across instances.
- The container/compose `HEALTHCHECK` calls `datadock -addr :8080 -healthcheck`,
  which exits non-zero when `/healthz` is unreachable.

## Data safety

- The default `memory` storage mode with a `-db` file saves a **plaintext**
  snapshot on shutdown; running in-memory only (`-db :memory:`) loses everything
  on exit. Both now log a startup warning.
- Use `-storage-mode disk` (with `-db` pointing at a **directory**) plus a
  32-byte `DATADOCK_ENCRYPTION_KEY` (hex or base64) to encrypt table files and
  stored secrets at rest.
- Back up with `GET /api/admin/snapshot` (Admin session). Note the snapshot
  contains secrets in the clear — store it accordingly.

# Antelope — docker-compose deployment

Runs the full Antelope stack from a **single image**. The Vue frontend is
embedded into the Go binary via `//go:embed`, so the server, API, and web UI
all ship in one container built from the repo's [`Dockerfile`](../Dockerfile).

## Services

| Service    | Image                | Purpose                                   | Host port |
|------------|----------------------|-------------------------------------------|-----------|
| `antelope` | built from `Dockerfile` | API + embedded web UI                  | `8086`    |
| `postgres` | `postgres:16`        | Primary database                          | —         |
| `redis`    | `redis:7-alpine`     | Cache, leader election, per-user storage creds | —    |
| `maildev`  | `maildev/maildev`    | Local SMTP catcher (dev email)            | `1080` (web UI) |
| `pgadmin`  | `dpage/pgadmin4`     | Web UI for the bundled PostgreSQL         | `5050` (web UI) |

**Nomad is external** and not managed here — Nextflow jobs are dispatched to
your own Nomad cluster. Point `ANTELOPE_NOMAD_HOST` / `ANTELOPE_NOMAD_PORT` at
it (default: a Nomad agent on the docker host via `host.docker.internal:4646`).

## Quick start

The stack needs three prerequisites before `docker compose up` succeeds:

1. **Docker** with BuildKit (default in recent versions) and the ability to build
   the image — the whole app (server + embedded web UI + built-in agent skills)
   is compiled from this repo's `Dockerfile`.
2. **The agent skill bundle** — `internal/modules/agent/skillbundle/bundle/`
   must exist on the host before `docker build` (it is generated, not committed).
   If it is missing, the Dockerfile stops early and tells you to run
   `make skills-bundle` (see [../AGENTS.md](../AGENTS.md) "Agent Skills").
3. **A reachable HashiCorp Nomad agent** — this is the step most people miss.
   The `antelope` service **panics and the container crash-loops** if it cannot
   reach Nomad on startup, even in debug mode with placeholder secrets. For a
   local demo, run a dev agent on the host before `docker compose up`:

   ```bash
   if ! command -v nomad >/dev/null; then brew install hashicorp/tap/nomad; fi
   nomad agent -dev -bind 0.0.0.0 &
   ```

   (The compose file defaults to `ANTELOPE_NOMAD_HOST=http://host.docker.internal`,
   so the dev agent must bind `0.0.0.0` — the default `127.0.0.1` is *not*
   reachable from the container.)

Then:

```bash
cd docker
cp .env.example .env        # edit secrets (JWT keys, DB password)
docker compose up -d --build
```

- App:        http://localhost:8086
- Maildev UI: http://localhost:1080
- pgAdmin:    http://localhost:5050 (login `admin@antelope.dev` / `pgadmin`; the
  `Antelope` server is pre-registered and auto-connects to the bundled postgres)

First boot seeds the single super-user from `system.super-user` in
[`config.yaml`](./config.yaml) with the password from
`ANTELOPE_SYSTEM_SUPER_USER_PASSWORD`.

## Configuration model

Config is **env-driven**. Resolution priority, highest first:

1. `ANTELOPE_*` environment variables (set in `docker-compose.yaml`, overridable via `.env`)
2. [`config.yaml`](./config.yaml) — mounted read-only at `/app/config.yaml`
3. Built-in defaults

`config.yaml` carries only **non-secret structural/list** values that are
awkward to express as env vars (Nomad datacenters, agent/skills blocks,
logging). Everything deployment-specific — hosts, passwords, signing keys,
email credentials — is supplied as `ANTELOPE_*` env and **overrides** the file.
The mapping is the usual Viper convention: nested key →
`ANTELOPE_<UPPER>` with `.`/`-` replaced by `_`
(e.g. `postgresql.password` → `ANTELOPE_POSTGRESQL_PASSWORD`,
`jwt.access-signing-key` → `ANTELOPE_JWT_ACCESS_SIGNING_KEY`). LLM access is
configured per-user in the UI (stored in Redis), not via env.

See [`.env.example`](./.env.example) for every overridable variable.

> The stack defaults to **debug** mode so it boots out of the box with the
> placeholder secrets. For any non-local deployment, set
> `ANTELOPE_SYSTEM_MODE=release` **and** provide real
> `ANTELOPE_JWT_*_SIGNING_KEY`, `POSTGRES_PASSWORD` and
> `ANTELOPE_SYSTEM_SUPER_USER_PASSWORD` — release mode fails fast if any of
> these is empty or left at a shipped placeholder.

## Volumes & mounts

- `postgres_data`, `redis_data` — named volumes for persistence.
- `./config.yaml` → `/app/config.yaml` (read-only).
- Built-in agent skills need **no mount**. The image is built with
  `-tags skills`, which compiles the generated skill bundle (~700 skills,
  ~23 MiB) into the binary; it is unpacked to `/app/skills/bundle` on first
  start. Regenerate it with `make skills-bundle` before `docker build` when the
  `skills/upstream/` submodules move.

> ⚠️ Per-user MinIO/S3 storage credentials are stored **only in Redis**. If you
> remove the `redis_data` volume, all users must re-enter their storage
> credentials and existing artifact previews will break.

## Common commands

```bash
docker compose logs -f antelope        # tail server logs
docker compose up -d --build antelope  # rebuild & restart just the app
docker compose down                    # stop (keep volumes)
docker compose down -v                 # stop and DELETE all data
```

## Connecting to a host Nomad agent

The `antelope` service has a `host.docker.internal:host-gateway` alias, so the
default `ANTELOPE_NOMAD_HOST=http://host.docker.internal` reaches a Nomad agent
bound on the docker host (works on Linux too). For a remote cluster, set the
full URL and an `ANTELOPE_NOMAD_TOKEN` if ACLs are enabled.

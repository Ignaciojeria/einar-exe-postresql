# ElectricSQL on this VM

Each project database gets its own ElectricSQL container automatically.

## Provisioning model

When `POST /projects` creates a database, the PostgreSQL API now also:

1. allocates an internal loopback port starting at `13100`,
2. generates a per-project Electric secret,
3. starts a Docker container named `electric-<db_name>`,
4. connects that Electric instance to the new project database,
5. stores the Electric metadata in `public.projects`.

Existing projects are backfilled lazily: the first authenticated call to `/projects/{project}/sync` will create/start Electric for that database if it is missing or unhealthy.

Electric containers are not exposed directly. They listen only on `127.0.0.1:<electric_port>` and use Docker `--restart unless-stopped`.

## Authenticated sync endpoint

Clients should go through the Go API, which validates the OpenID Connect bearer token, verifies project ownership, and injects the internal Electric secret server-side.

`table` must be the plain table name. The backend injects the tenant schema automatically.

```http
GET https://postgresql.exe.xyz/projects/{project}/sync?table=todos&offset=-1
Authorization: Bearer <oidc-token>
```

Example:

```http
GET https://postgresql.exe.xyz/projects/my-project/sync?table=todos&offset=-1
Authorization: Bearer <oidc-token>
```

Local development call when `AUTH_DISABLED=true`:

```bash
curl -i \
  -H 'X-Dev-Sub: <owner-sub>' \
  'http://127.0.0.1:8000/projects/<project>/sync?table=todos&offset=-1'
```

## Operations

List Electric containers:

```bash
docker ps --filter 'name=electric-'
```

Inspect a project's Electric metadata:

```bash
sudo -u postgres psql -Atc \
  "select name, db_name, electric_port, electric_status, electric_container from public.projects order by created_at desc;"
```

Container logs:

```bash
docker logs <electric-container-name> --tail 100
```

## PostgreSQL prerequisites

These settings were applied on the VM:

```sql
ALTER SYSTEM SET wal_level = logical;
ALTER SYSTEM SET max_replication_slots = 10;
ALTER SYSTEM SET max_wal_senders = 10;
```

A dedicated PostgreSQL role named `electric` is used by ElectricSQL.

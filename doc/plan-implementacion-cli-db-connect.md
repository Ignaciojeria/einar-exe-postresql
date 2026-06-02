# Plan de implementación: `einar db connect` + persistencia de datos de provisioning

## Contexto
Actualmente el provisioning de proyecto/base de datos devuelve una respuesta como:

```json
{
  "id": "c82b531b-b8d5-4daf-83a7-063f153e4e58",
  "name": "mi-proyecto-02",
  "schema": "mi_proyecto_02_db",
  "databaseUrl": "postgres://mi_proyecto_02_user:<password>@localhost:5432/postgres?sslmode=disable"
}
```

`schema` es informativo para el CLI y puede persistirse en config, pero la **fuente de verdad para conexión sigue siendo `databaseUrl`**.

Y existe el túnel WebSocket autenticado en:

- `GET /projects/{project}/tunnel`

Objetivo: que el usuario pueda ejecutar **`./einar db connect`** y conectar a su DB sin fricción (psql/DBeaver), reutilizando sesión/token del CLI.

---

## Arquitectura real (3 máquinas/servicios separados)

1. **Auth provider** (emite/valida identidad):
   - `https://einar.exe.xyz/`
2. **PostgreSQL API** (provisioning + túnel DB):
   - `https://postgresql.exe.xyz:8000`
3. **VM del usuario** (runtime del proyecto), DNS indicado por usuario:
   - `https://<dns-vm-usuario>`

Todos son entornos separados.

### Topología para `db connect`

```text
Cliente SQL (DBeaver/psql)
        │
        ▼
localhost:15432 (host local)
        │
        ▼
CLI `einar db connect`
        │ Bearer token (emitido por einar.exe.xyz)
        │ WebSocket a postgresql.exe.xyz:8000/projects/{project}/tunnel
        ▼
PostgreSQL API (postgresql.exe.xyz)
        │ TCP interno a DB backend
        ▼
Instancia PostgreSQL real
```

**Importante:** para `db connect`, el endpoint principal es `postgresql.exe.xyz:8000`.
La VM del usuario no es parte obligatoria de este path de túnel.

---

## Consideraciones explícitas de Mutagen

1. Mutagen se sincroniza con la **VM en el DNS indicado por el usuario**.
2. Mutagen sincroniza archivos/código entre host y VM.
3. Mutagen **no usa** `databaseUrl` ni metadata DB para establecer conexión a PostgreSQL.
4. `db connect` no depende de Mutagen para funcionar, ya que opera contra `postgresql.exe.xyz:8000`.
5. `.einar/config.json` puede sincronizarse como archivo normal si está dentro de la ruta sincronizada; decidir según seguridad si conviene excluirlo.
---

## Objetivos funcionales

1. Guardar metadata de provisioning en config local del CLI tras `init`.
2. Mantener compatibilidad con el flujo actual (sin romper usuarios existentes).
3. Exponer un comando simple:
   - `./einar db connect`
4. Levantar túnel local automáticamente y mostrar datos de conexión consumibles por psql/DBeaver.
5. Mantener configuración mínima para evitar ruido en `config.json`.

---

## Decisiones de diseño

### 1) Configuración local (mínima)
Guardar en `.einar/config.json` solo datos necesarios de proyecto/DB:

```json
{
  "project": {
    "id": "c82b531b-b8d5-4daf-83a7-063f153e4e58",
    "name": "mi-proyecto-02"
  },
  "database": {
    "url": "postgres://mi_proyecto_02_user:<password>@localhost:5432/postgres?sslmode=disable",
    "schema": "mi_proyecto_02_db"
  },
  "configVersion": 1
}
```

> Nota: para MVP se permite `database.url` completa (incluye password). En v2 se recomienda mover password a keychain/secret store y dejar solo metadata no sensible.

### 2) Fuente de token para el CLI
Orden recomendado:
1. Token de sesión local (`einar login`)
2. Variable de entorno (`EINAR_TOKEN` o `PGTUNNEL_TOKEN`)
3. Flag `--token`

### 3) Resolución de endpoints (sin persistirlos en config)
Para evitar ruido, el CLI **no genera bloque `endpoints`** en `config.json`.

Resolución sugerida por comando:
1. Flag (ej. `--api`, `--auth-url`, `--vm-url`)
2. Variable de entorno
3. Default interno del CLI

Defaults recomendados:
- Auth: `https://einar.exe.xyz`
- Postgres API: `https://postgresql.exe.xyz:8000`
- VM: sin default global (se define cuando el comando de runtime la requiera)

### 4) Puerto local del túnel
- Default: `127.0.0.1:15432`
- Si está ocupado, intentar siguiente libre (`15433`, `15434`, ...), salvo `--port` forzado.

### 5) Compatibilidad
- Soportar formato legacy de config mientras se migra.
- Migración automática al nuevo schema al ejecutar `init` o `db connect`.

---

## Cambios por componente

## A. CLI `init` (provisioning DB)

### Tareas
1. Consumir respuesta de `POST /projects` en Postgres API.
2. Persistir `project.id`, `project.name`, `database.url` y opcionalmente `database.schema`.
3. Merge no destructivo con config existente + `configVersion`.
4. Imprimir resumen:
   - proyecto
   - db name/user (parseados)
   - endpoint Postgres API efectivo
   - siguiente paso: `./einar db connect`

### Criterios de aceptación
- Tras `init`, existe `.einar/config.json` con estructura mínima esperada.
- No se pierde compatibilidad con config legacy.

---

## B. Nuevo comando `db connect`

### UX esperada
```bash
./einar db connect
```

Acciones:
1. Leer proyecto desde config.
2. Resolver token (sesión/env/flag).
3. Resolver endpoint Postgres API (`--api` > env > default).
4. Levantar túnel a `GET /projects/{project}/tunnel`.
5. Exponer socket local (`127.0.0.1:<port>`).
6. Mostrar instrucciones para psql/DBeaver.

### Flags sugeridas
- `--project <name>`
- `--port <n>` (default 15432)
- `--token <jwt>`
- `--api <postgres-api-url>`
- `--open psql` (fase 2)

### Criterios de aceptación
- Con token válido emitido por `einar.exe.xyz`, túnel operativo a través de `postgresql.exe.xyz:8000`.
- Cliente local puede ejecutar `select 1` por `localhost:<port>`.

---

## C. Reuso/refactor de `cmd/pgtunnel`

### Estrategia
1. Extraer lógica de túnel a paquete común (ej. `internal/tunnel`).
2. Reusar en `cmd/pgtunnel` y `db connect`.
3. Evitar duplicación en dial WS, piping y shutdown.

### Criterios de aceptación
- `cmd/pgtunnel` mantiene comportamiento actual.
- `db connect` reutiliza el mismo core.

---

## D. API (mejora opcional recomendada)

En `POST /projects`, mantener `databaseUrl`, exponer `schema`, y opcionalmente agregar ayuda para uso local:

```json
{
  "id": "...",
  "name": "demo",
  "schema": "demo_db",
  "databaseUrl": "postgres://demo_user:pass@host:5432/postgres?sslmode=disable",
  "tunnel": {
    "recommendedLocalHost": "127.0.0.1",
    "recommendedLocalPort": 15432,
    "command": "einar db connect"
  }
}
```

Notas importantes para el CLI:
- no asumir que el nombre de la base coincide con el proyecto;
- no reemplazar `/postgres` por `/<schema>` al preparar la URL local del túnel;
- no hardcodear `public` en migraciones/SQL ni en clientes que usen el schema del proyecto.

Beneficio: evita confusión entre URL interna del backend y conexión local del cliente.

---

## Plan de ejecución (iterativo)

### Fase 1 (MVP)
1. Persistir respuesta de provisioning (`database.url`) en config.
2. Implementar `db connect` contra Postgres API.
3. Output listo para psql/DBeaver.

### Fase 2 (hardening)
1. Mover password a keychain/secret store.
2. Mantener en config solo metadata no sensible.
3. Opcional: comandos `db tunnel stop/list/logs`.

### Fase 3 (DX avanzada)
1. `--open psql`.
2. Mejor diagnóstico de conectividad/token/audience.
3. Refresh de sesión automático (si aplica).

---

## Riesgos y mitigaciones

1. **Credenciales en texto plano en config**
   - Mitigación MVP: `.einar/` en `.gitignore`, warnings.
   - Mitigación final: keychain.

2. **Confusión entre múltiples endpoints**
   - Mitigación: defaults claros + flags/env explícitos + help por comando.

3. **Token expirado o audience incorrecta**
   - Mitigación: error claro (`run einar login`) + validación de claims esperadas por Postgres API.

4. **Puerto local ocupado**
   - Mitigación: fallback automático o error accionable con `--port`.

5. **Suposición incorrecta sobre Mutagen**
   - Mitigación: documentar que Mutagen sincroniza con VM (DNS usuario) pero no gobierna el túnel DB.

---

## Validación de extremo a extremo

### Escenario 1: producción separada (recomendado)
1. Login contra auth provider (`einar.exe.xyz`).
2. Provisioning en Postgres API (`postgresql.exe.xyz:8000`).
3. Ejecutar `./einar db connect --api https://postgresql.exe.xyz:8000`.
4. Validar consulta con psql vía `localhost:15432`.

### Escenario 2: con VM de usuario + Mutagen activo
1. Verificar sync Mutagen hacia VM (DNS usuario).
2. Confirmar runtime VM operativo (independiente de DB connect).
3. Ejecutar `db connect` y verificar que no depende de la VM para abrir túnel DB.

Comando de prueba:

```bash
PGPASSWORD='<password>' psql -h localhost -p 15432 -U mi_proyecto_02_user -d postgres -c 'show search_path; select current_database(), current_user;'
```

DBeaver:
- Host: `localhost`
- Port: `15432`
- Database: `postgres`
- Username: `mi_proyecto_02_user`
- SSL: disable

---

## Entregables

1. Cambios en CLI `init` para persistir provisioning de DB.
2. Nuevo comando `db connect` funcional contra Postgres API.
3. Refactor mínimo para reusar lógica de túnel.
4. Documentación de uso en `README`/`doc` con sección explícita de arquitectura separada y Mutagen.

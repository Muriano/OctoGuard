# OctoGuard v1: plan de implementación

> **Para agentes que lo ejecuten:** SUB-SKILL REQUERIDA: usa superpowers:subagent-driven-development (recomendado) o superpowers:executing-plans para implementar este plan tarea a tarea. Los pasos usan checkboxes (`- [ ]`). Cada tarea tiene un issue en GitHub: ciérralo desde el PR (`Closes #N`).

**Objetivo:** construir un binario Go `octoguard` (CLI + demonio local) y unas skills de arranque que permitan a los agentes leer y esperar recursos de GitHub gastando la mínima cuota, sin topar nunca con el rate limit.

**Arquitectura:** el CLI resuelve el token y habla con un demonio por socket Unix (JSON-RPC sencillo). El demonio es dueño de una caché SQLite (ETag + cuerpo + estado de cuota) separada por `(host, hash del token)`, deduplica peticiones en vuelo y agrupa los waiters por recurso. También decide entre REST y GraphQL y aplica un gobernador de cuota. Las guías y la skill van embebidas en el binario (`embed`) y se exponen mediante `agent-context`, `guide` y `skills install`.

**Stack:** Go 1.23+, `github.com/google/go-github/v66` (REST), `github.com/shurcooL/githubv4` (GraphQL), `modernc.org/sqlite` (sin cgo), `github.com/spf13/cobra` (CLI), `github.com/charmbracelet/lipgloss` (stats), goreleaser y tap de Homebrew `Muriano/homebrew-tap`.

**Spec:** `docs/design/2026-11-11-octoguard-v1-design.md`

## Restricciones globales

- Módulo Go: `github.com/Muriano/OctoGuard`. Binario: `octoguard`.
- Repo público, licencia MIT.
- Precedencia del token: `GH_TOKEN` → `GITHUB_TOKEN` → `gh auth token`. El token nunca se escribe en disco, logs, stdout ni caché.
- Socket: `$XDG_RUNTIME_DIR/octoguard.sock` o, si no existe, `os.UserCacheDir()/octoguard/octoguard.sock`, con permisos `0600` y directorio `0700`.
- Caché: `os.UserCacheDir()/octoguard/cache.db` en modo WAL.
- Claves de caché y cuota prefijadas con `host` + `sha256(token)[:16]`. En v1 el host siempre es `github.com`.
- Salida por defecto: JSON estable con campo `"schema_version": 1`. `--format text` es opcional.
- Códigos de salida: `0` ok · `1` error · `2` uso incorrecto · `3` `wait` vencido con estado `pending` · `4` merge rechazado por checks.
- `wait --timeout` por defecto `8m`. El demonio mantiene el poller vivo durante `2m` de gracia después de que se vaya el último waiter.
- Fallback sin demonio: `wait` sondea en proceso con un intervalo mínimo de `60s`.
- Baseline de ahorro de `wait` en stats: `30s` (configurable con `OCTOGUARD_POLL_BASELINE`).
- Plataformas: darwin y linux, amd64 y arm64.

## Foco de revisión

- **Token cambiado o revocado a mitad de sesión.** Un 401 no puede envenenar la caché del token anterior. Se responde con un error claro y el siguiente token usa su propio espacio de claves (tarea 4, `TestCache_IsolatedByToken`, y tarea 6, `Test401NotCached`).
- **Dos CLIs arrancando el demonio a la vez.** Tiene que acabar habiendo exactamente un demonio, y ningún CLI puede quedarse colgado (tarea 7, `TestDaemon_ConcurrentAutostartSingleInstance`).
- **El agente mata el CLI a mitad de `wait`** (por timeout de su herramienta). El demonio no puede dejar pollers huérfanos más allá del periodo de gracia (tarea 9, `TestWait_ClientDisconnectReleasesWaiter`).
- **PR sin checks requeridos o con checks `skipped`/`neutral`.** `checks-green` no puede bloquear para siempre (tarea 9, `TestChecksGreen_NoRequiredChecksUsesAll` y `TestChecksGreen_SkippedNeutralCount`).
- **Ref ambigua** (`123` fuera de un repo git, o una URL de otro host). Debe devolver un error de uso claro (código 2) y no adivinar (tarea 3, `TestParseRef_*`).

---

## Estructura de ficheros

```
cmd/octoguard/main.go             # entrypoint
internal/cli/                     # comandos cobra (uno por fichero: get.go, wait.go, quota.go, write.go, stats.go, guide.go, skills.go, daemon.go)
internal/ref/ref.go               # parseo de <ref>
internal/auth/token.go            # resolución de token + hash
internal/cache/cache.go           # SQLite: entradas, cuota, contadores
internal/quota/governor.go        # estado de cuota desde cabeceras + decisiones
internal/gh/client.go             # transporte HTTP con ETag, cabeceras de cuota, REST y GraphQL
internal/gh/resources.go          # lecturas tipadas (pr, issue, reviews, comments, checks, runs)
internal/route/route.go           # elección REST vs GraphQL
internal/daemon/server.go         # socket, autoarranque, single-flight, idle shutdown
internal/daemon/rpc.go            # tipos de petición/respuesta
internal/wait/conditions.go       # evaluación de condiciones
internal/wait/poller.go           # poller por recurso, agrupando waiters
internal/write/write.go           # escrituras + invalidación + espaciado
internal/stats/stats.go           # agregados y cálculo de ahorro
internal/stats/render.go          # render gráfico
internal/guides/*.md + embed.go   # guías embebidas
internal/skills/SKILL.md + install.go
testdata/fakegh/                  # servidor GitHub falso para tests
```

---

### Tarea 1: Esqueleto del repo, CI y servidor GitHub falso

**Ficheros:** `go.mod`, `cmd/octoguard/main.go`, `internal/cli/root.go`, `testdata/fakegh/server.go`, `.github/workflows/ci.yml`, `LICENSE`, `README.md`, `Makefile`

**Interfaces:**
- Produce: `fakegh.New(t *testing.T) *fakegh.Server` con `URL string`, `SetResource(path string, body []byte, etag string)`, `SetRate(kind string, remaining, limit int, reset time.Time)`, `Calls(path string) int`, `GraphQLCalls() int`. Responde `304` si `If-None-Match` coincide y emite cabeceras `X-RateLimit-*`. Puede devolver 403 o 429 con `Retry-After` bajo demanda (`FailNext(path, status, retryAfter)`).
- Produce: `cli.NewRoot() *cobra.Command`, con flags globales `--format json|text` y `--api-url` (oculto, para tests).

- [ ] Test `TestFakeGH_ETag304`: segunda petición con el ETag devuelto → 304 y `Calls == 2`.
- [ ] Test `TestRoot_Version`: `octoguard --version` imprime la versión inyectada con `-ldflags`.
- [ ] Implementar. CI con `go vet`, `go test -race ./...` y `golangci-lint`.
- [ ] `make test` pasa. Commit.

### Tarea 2: Resolución de token

**Ficheros:** `internal/auth/token.go`, `internal/auth/token_test.go`

**Interfaces:**
- Produce: `auth.Resolve(ctx) (Token, error)`, con `type Token struct{ Value string; Source string; ID string }`. `ID` es `hex(sha256(Value))[:16]`. `Token.String()` devuelve `"***"`.

- [ ] Tests: `TestResolve_PrefersGHToken`, `TestResolve_FallsBackToGitHubToken`, `TestResolve_FallsBackToGhCLI` (inyectando un `execCommand` falso), `TestResolve_NoTokenError` (el mensaje sugiere `gh auth login`) y `TestToken_StringRedacted`.
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 3: Parseo de refs

**Ficheros:** `internal/ref/ref.go`, `internal/ref/ref_test.go`

**Interfaces:**
- Produce: `ref.Parse(s string, kind Kind, cwd string) (Ref, error)`, con `type Ref struct{ Host, Owner, Repo string; Number int; Kind Kind }` y `Kind` ∈ `PR|Issue|Run`. `Ref.Key() string` devuelve `"github.com/owner/repo/pr/123"`.

- [ ] Tests: `owner/repo#12`, `https://github.com/o/r/pull/12`, `.../issues/12`, `.../actions/runs/99` y `12` dentro de un repo git (remote `origin` https y ssh). Casos de error: `TestParseRef_BareNumberOutsideRepo` → `ErrUsage`, `TestParseRef_OtherHostRejected` (por ejemplo `ghe.example.com`) → `ErrUsage` con un mensaje que diga que v1 solo soporta github.com.
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 4: Caché SQLite

**Ficheros:** `internal/cache/cache.go`, `internal/cache/cache_test.go`

**Interfaces:**
- Produce: `cache.Open(path string) (*Cache, error)` y estos métodos:
  - `Get(tokenID, key string) (Entry, bool, error)`
  - `Put(tokenID, key string, e Entry) error`
  - `Invalidate(tokenID string, keys ...string) error`
  - `InvalidatePrefix(tokenID, prefix string) error`
- `type Entry struct{ Body []byte; ETag, LastModified string; FetchedAt time.Time; Via string }`, donde `Via` es `"rest"` o `"graphql"`.
- Tablas de contadores para stats: `IncCounter(tokenID, name string, n int64, at time.Time)` agregada por hora, y `Counters(tokenID string, since time.Time) ([]Bucket, error)`.

- [ ] Tests: put/get, `TestCache_IsolatedByToken` (la misma key con distinto tokenID no se ve), `InvalidatePrefix` borra `.../pr/12/*`, `TestCache_ConcurrentWriters` (dos `*Cache` sobre el mismo fichero sin `SQLITE_BUSY`, usando WAL y `busy_timeout`), y `TestCache_FileMode0600`.
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 5: Gobernador de cuota

**Ficheros:** `internal/quota/governor.go`, `internal/quota/governor_test.go`

**Interfaces:**
- Produce: `quota.New(now func() time.Time) *Governor`.
- `Observe(tokenID string, h http.Header)` lee `X-RateLimit-Resource/Remaining/Limit/Reset/Used` y, si viene en `X-RateLimit-Resource`, el recurso `graphql`.
- `Snapshot(tokenID) map[string]Bucket`, con `Bucket{Remaining, Limit int; Reset time.Time; Known bool}`.
- `Allow(tokenID, resource string, cost int) (wait time.Duration)` devuelve 0 si se puede gastar ya; si no, cuánto esperar.
- `PollInterval(tokenID, resource string, base time.Duration) time.Duration` escala el intervalo según el presupuesto.
- `ObserveRetryAfter(tokenID, resource string, d time.Duration)`.
- Umbrales: si queda más del 50 %, intervalo `base`. Entre el 50 y el 20 %, `base×2`. Entre el 20 y el 5 %, `base×4`. Por debajo del 5 %, solo se permiten lecturas con ETag que ya estén en caché, a un ritmo de `remaining / segundos_hasta_reset`.

- [ ] Tests: cada umbral, `Retry-After` respetado, y que `Snapshot` funcione sin red.
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 6: Cliente GitHub con ETag y lecturas tipadas

**Ficheros:** `internal/gh/client.go`, `internal/gh/resources.go`, `internal/route/route.go` y sus tests

**Interfaces:**
- Consume: `cache.Cache`, `quota.Governor`, `auth.Token`, `ref.Ref`.
- Produce: `gh.NewClient(baseURL string, tok auth.Token, c *cache.Cache, g *quota.Governor, counters CounterSink) *Client`.
  - `(*Client).Get(ctx, r ref.Ref, what Resource, opt GetOpts) (Result, error)`.
  - `Resource` ∈ `PR|Issue|Reviews|Comments|Checks|Runs|RequiredChecks`.
  - `GetOpts{Fresh bool; MaxAge time.Duration}`.
  - `Result{Data json.RawMessage; FromCache bool; NotModified bool; Via string; FetchedAt time.Time}`.
- Produce: `route.Choose(what Resource, cached bool, snap map[string]quota.Bucket) Via`. Elige REST si el recurso ya está en caché (para revalidar con ETag) o si a GraphQL le queda menos del 20 %. Elige GraphQL para lecturas compuestas (`PR` completo = pr+reviews+checks) sin caché, o si a REST le queda menos del 20 %.
- `Checks` combina check-runs y commit statuses del head SHA.
- Contadores: `cache_hit`, `etag_304`, `rest_call`, `graphql_points`, `ratelimit_hit`.

- [ ] Tests con fakegh: `TestGet_SecondCallIs304AndCountsSaving`, `TestGet_MaxAgeServesFromCacheNoCall`, `TestGet_FreshBypassesMaxAge`, `TestRoute_PrefersGraphQLWhenRestLow`, `TestChecks_MergesRunsAndStatuses`, `Test401NotCached` (el 401 no escribe la entrada y devuelve un error que sugiere revisar el token) y `TestGet_403RetryAfterFeedsGovernor`.
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 7: Demonio, socket, autoarranque y single-flight

**Ficheros:** `internal/daemon/server.go`, `internal/daemon/rpc.go`, `internal/daemon/client.go`, `internal/cli/daemon.go` y sus tests

**Interfaces:**
- Consume: `gh.Client` (uno por tokenID, creado bajo demanda).
- Produce: `daemon.Request{Op string; Token string; Args json.RawMessage}` y `daemon.Response{OK bool; Data json.RawMessage; Err string; Code int}`, en JSON delimitado por líneas sobre el socket Unix. Las operaciones son `get`, `wait`, `write`, `quota`, `stats` y `status`.
- Produce: `daemon.Dial(ctx) (*Conn, error)`. Si no hay socket, lanza `octoguard daemon run` desacoplado (`setsid`) con un lock de fichero `octoguard.lock` y reintenta durante 3 s.
- Produce: `daemon.Run(ctx, Config{IdleTimeout: 10*time.Minute})`.
- Single-flight por `(tokenID, key)` con `golang.org/x/sync/singleflight`.
- `octoguard daemon run|status|stop`.

- [ ] Tests: `TestDaemon_ConcurrentAutostartSingleInstance` (10 goroutines llamando a `Dial` → 1 proceso y todas conectan), `TestDaemon_SingleFlightDedup` (5 `get` simultáneos del mismo PR → fakegh `Calls == 1` y contador `dedup == 4`), `TestDaemon_IdleShutdown`, `TestDaemon_SocketPerms0600` y `TestDaemon_TokenNotPersisted` (grep del token en `cache.db` y en los logs → 0 coincidencias).
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 8: Comandos `get` y `quota` (con fallback sin demonio)

**Ficheros:** `internal/cli/get.go`, `internal/cli/quota.go` y sus tests

**Interfaces:**
- Consume: `daemon.Dial`, `gh.Client` (fallback directo), `ref.Parse`, `auth.Resolve`.
- CLI: `octoguard get pr|issue|reviews|comments|checks|runs <ref> [--fresh] [--max-age 30s]` y `octoguard quota`.
- Salida de `get`: `{"schema_version":1,"ref":...,"data":...,"meta":{"from_cache":bool,"not_modified":bool,"via":"rest|graphql","fetched_at":...}}`.
- Salida de `quota`: `{"schema_version":1,"core":{...},"graphql":{...},"search":{...}}`. Si un bucket no es `Known`, hace como mucho una llamada a `/rate_limit` (que no consume cuota) y lo indica en `meta.probed`.

- [ ] Tests: `TestGetCLI_UsesDaemon`, `TestGetCLI_FallbackWhenDaemonUnavailable` (stderr contiene `octoguard: daemon unavailable, direct call` y aun así sale con 0), `TestQuota_NoAPICallWhenKnown` y `TestQuota_ProbesOnceWhenUnknown`.
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 9: `wait` con condiciones y poller compartido

**Ficheros:** `internal/wait/conditions.go`, `internal/wait/poller.go`, `internal/cli/wait.go` y sus tests

**Interfaces:**
- Consume: `gh.Client.Get` (con `Checks` y `RequiredChecks`), `quota.Governor.PollInterval`.
- Produce: `wait.Evaluate(cond Condition, prev, cur Snapshot) (met bool, changes []Change)`.
  - Condiciones de PR: `checks-done`, `checks-green`, `review`, `comment`, `merged`, `closed`, `changed`.
  - Condiciones de issue: `comment`, `closed`, `labeled`, `changed`.
  - Run: `done`.
- Semántica de checks (spec, decisiones tras la revisión):
  - `done` significa que nada está en `queued`, `in_progress` o `pending`.
  - `green` significa que todos los checks requeridos están en `success`, `neutral` o `skipped`. Si no hay checks requeridos, se exige lo mismo a todos.
  - Si alguno está en `failure`, `cancelled` o `timed_out`, la condición se resuelve como no cumplible y la respuesta es `{"status":"failed"}` con salida 0, para que el agente reaccione.
- Produce: `wait.Pool` con un poller por `(tokenID, ref.Key())` y N waiters suscritos. El intervalo base es 15 s para checks y runs y 30 s para el resto, ajustado por el gobernador. El periodo de gracia es de 2 min.
- CLI: `octoguard wait pr|issue|run <ref> --until <cond> [--timeout 8m]`. Devuelve `{"status":"met|pending|failed","condition":...,"changes":[...],"state":...}`. `pending` sale con código 3.
- Contador `wait_saved`: `ceil(duración/baseline) − polls_reales_atribuibles`.

- [ ] Tests: `TestChecksGreen_NoRequiredChecksUsesAll`, `TestChecksGreen_SkippedNeutralCount`, `TestChecksGreen_FailureReturnsFailed`, `TestWait_TimeoutReturnsPendingExit3`, `TestWait_TwoWaitersOnePoller` (fakegh `Calls` igual al número de ciclos, no al doble), `TestWait_ClientDisconnectReleasesWaiter` (el poller se para tras la gracia) y `TestWait_ResumeWithinGraceNoExtraCall`.
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 10: Escrituras con invalidación y espaciado

**Ficheros:** `internal/write/write.go`, `internal/cli/write.go` y sus tests

**Interfaces:**
- Consume: `gh.Client` (que debe exponer `Do(ctx, method, path string, body any) (json.RawMessage, error)` para REST y `Mutate(ctx, m any, input any) error` para GraphQL), `cache.InvalidatePrefix`, `wait.Evaluate`.
- Comandos: `pr create|comment|edit|merge`, `issue comment|create` y `project item-add|item-edit`. Los flags imitan a `gh` en lo esencial (`--title`, `--body`, `--body-file`, `--base`, `--head`, `--label`, `--add-label`, `--merge|--squash|--rebase`, `--project`, `--field`, `--value`).
- Espaciado: como mucho 1 escritura de creación de contenido por segundo por token y 60 por minuto. Lo que exceda se encola en el demonio, no se rechaza.
- Merge: lee `Checks` y `RequiredChecks` con `Fresh`. Si `checks-green` no se cumple, sale con código 4 y `{"status":"rejected","reason":...,"hint":"octoguard wait pr <ref> --until checks-green"}`. Si se cumple, llama a la API de merge.
- Invalidación: la escritura sobre el PR `N` hace `InvalidatePrefix(".../pr/N/")`, y lo mismo con issues.

- [ ] Tests: `TestComment_InvalidatesComments`, `TestMerge_RejectsWhenChecksRed` (código 4 y sin llamada a merge), `TestMerge_FreshReadIgnoresCache`, `TestWrites_SpacedUnderSecondaryLimit` (con un reloj falso, 70 comentarios → ≥ 60 s simulados) y `TestProjectItemEdit_GraphQLMutation`.
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 11: `stats` gráfico

**Ficheros:** `internal/stats/stats.go`, `internal/stats/render.go`, `internal/cli/stats.go` y sus tests

**Interfaces:**
- Consume: `cache.Counters`, `quota.Governor.Snapshot`.
- Produce: `stats.Compute(buckets []cache.Bucket, snap map[string]quota.Bucket, since time.Duration) Report`.
  - `Report{Quota; Saved{Cache, ETag304, Dedup, WaitVsPoll, TotalCalls, RESTSaved, GraphQLPointsSaved}; Hourly []HourPoint{Real, Avoided}; RateLimits{Avoided, Suffered}; Baseline time.Duration}`.
- Produce: `stats.Render(w io.Writer, r Report, width int, color bool)`. Pinta barras de cuota, barras de ahorro por mecanismo, sparklines por hora y la línea de rate limits, como el mockup aprobado en la conversación de diseño.
- CLI: `octoguard stats [--since 1h|24h|7d|30d] [--live] [--format json]`. `--live` re-renderiza cada 2 s leyendo solo la SQLite y el demonio, sin red. `guide stats` explica cómo se calcula el ahorro.

- [ ] Tests: `TestCompute_SavingsFormula` (valores fijos → totales exactos), `TestRender_Golden` (salida en `testdata/stats_80cols.golden`), `TestRender_NoColorAndNarrow` (`NO_COLOR=1`, ancho 40, sin códigos ANSI ni líneas de más de 40 columnas) y `TestStatsLive_NoNetwork` (fakegh `Calls == 0` durante `--live`).
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 12: Autodescubrimiento: `agent-context` y `guide`

**Ficheros:** `internal/cli/agentcontext.go`, `internal/cli/guide.go`, `internal/guides/{quickstart,waiting,reading,quota,writes,anti-patterns,stats}.md`, `internal/guides/embed.go`

**Interfaces:**
- Produce: `octoguard agent-context`, que genera JSON a partir del árbol cobra: `{"schema_version":1,"version":...,"commands":[{"path":"wait pr","args":...,"flags":[...],"exit_codes":{...},"output_schema":...}]}`.
- Produce: `octoguard guide [tema]`. Sin tema, lista los disponibles. Cada guía empieza con `<!-- octoguard vX.Y.Z -->`.
- La guía `anti-patterns` prohíbe explícitamente: `sleep N && gh …`, `gh pr checks --watch`, `gh api rate_limit` y `gh api user` como chequeo de login. Para cada caso da su sustituto.

- [ ] Tests: `TestAgentContext_CoversAllCommands` (cada comando cobra visible aparece), `TestAgentContext_Stable` (golden) y `TestGuide_AllTopicsEmbeddedAndVersioned`.
- [ ] Redactar las guías (cortas: cada una cabe en una pantalla).
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 13: Skill de arranque e instalación multi-agente

**Ficheros:** `internal/skills/SKILL.md`, `internal/skills/install.go`, `internal/cli/skills.go`, `skills/octoguard/SKILL.md` (copia publicada para instalar con herramientas del ecosistema)

**Interfaces:**
- `SKILL.md` en el estándar de Agent Skills. El frontmatter tiene `name: octoguard` y una `description` que se dispara ante cualquier lectura, espera o polling sobre GitHub (PRs, issues, checks, runs) o cualquier mención de rate limit. El cuerpo tiene menos de 40 líneas: regla dura, 4 comandos clave y "ejecuta `octoguard guide quickstart`".
- Produce: `octoguard skills install [--agent claude|codex|gemini|copilot|all] [--scope user|project]` y `skills update`. El builder debe verificar las rutas vigentes de cada agente al implementar. Las de partida son `~/.claude/skills/octoguard/`, `~/.codex/skills/octoguard/`, `~/.gemini/skills/octoguard/` y `.github/skills/octoguard/` (Copilot, project scope). `update` sobrescribe solo si el fichero instalado tiene la cabecera `<!-- managed by octoguard -->`.

- [ ] Tests: `TestSkill_FrontmatterValid`, `TestSkill_UnderLineBudget`, `TestInstall_AllAgentsInTempHome` y `TestUpdate_DoesNotClobberUnmanaged`.
- [ ] Implementar, pasar los tests y hacer commit.

### Tarea 14: Release con goreleaser y tap de Homebrew

**Ficheros:** `.goreleaser.yaml`, `.github/workflows/release.yml`, `README.md` (instalación y uso)

**Interfaces:**
- Release al hacer push de un tag `v*`. Binarios darwin/linux amd64/arm64, checksums y fórmula en `Muriano/homebrew-tap`. El token del tap va en el secret `HOMEBREW_TAP_TOKEN`, con alcance mínimo: contents write solo sobre ese repo. Las actions se fijan por SHA.
- La fórmula ejecuta `octoguard --version` en `test do`.

- [ ] `goreleaser check` y `goreleaser release --snapshot --clean` en local sin errores.
- [ ] Crear `Muriano/homebrew-tap` si no existe (se confirma con el usuario antes).
- [ ] Commit.

### Tarea 15: Medición del éxito

**Ficheros:** `scripts/recon/gh_recon.py`, `docs/measuring.md`

- [ ] Versionar el script de reconocimiento usado en el diseño. Cuenta invocaciones `gh`, patrones de `sleep`+`gh`, `--watch` y errores de rate limit en las transcripciones locales de Claude Code y Codex, en una ventana de N días.
- [ ] Documentar el procedimiento: baseline (el diseño: ~18.700 llamadas `gh` en 14 días en Claude Code, 261 errores de rate limit, 293 `--watch`), y repetir a las 2 semanas de uso, junto con `octoguard stats --since 14d`. Criterio: 0 errores de rate limit sufridos.
- [ ] Commit.

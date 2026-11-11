# OctoGuard v1: diseño

Fecha: 2026-11-11 · Estado: borrador para aprobación

## 1. Intención y porqué

Trabajo con varios agentes de IA a la vez (Claude Code, Codex, Gemini, Copilot), lanzados a mano o por debajo de orquestadores propios, y todos comparten el mismo token de GitHub. Varias veces al día alguno agota la cuota de la API y **todo queda bloqueado**. En la práctica, cinco minutos parado es perder competitividad.

**Éxito = cero paradas por rate limit en el uso diario multi-agente**, sin que los agentes pierdan frescura en los datos que importan.

El problema de fondo es de comportamiento: por contexto se le puede pedir al agente que no haga polling, pero acaba volviendo a hacerlo. OctoGuard le da **una herramienta mejor que el bucle** (una llamada que espera de forma eficiente) más una skill que le enseña a usarla.

### Evidencia (reconocimiento local, últimos 14 días, transcripciones de Claude Code + Codex)

- ~18.700 invocaciones de `gh` solo desde Claude Code; volumen similar en Codex.
- 261 errores de rate limit (primario o secundario) en transcripciones.
- Lecturas dominan: `gh pr view` ~1.900, `gh issue view` ~1.400, `gh api .../pulls/N` ~890, `.../reviews` ~800+, comments, checks, runs.
- Polling explícito: 293 `gh pr checks --watch`, 1.761 `sleep 30`, 517 `sleep 60`, mayoritariamente junto a llamadas `gh`.
- Cuota gastada en mirar la cuota: 356 `gh api rate_limit` (Claude), ~3.900 `gh api user` (Codex).
- `gh pr view` / `gh issue view` / `gh project item-*` usan GraphQL; `gh api repos/...` usa REST. Hoy el reparto entre los dos cupos es accidental.
- Escrituras relevantes pero no derrochadoras: `pr create` 461, `project item-edit` 378, `pr comment`, `pr edit`, `pr merge`.

## 2. Objetivos (v1)

1. **`wait`**: sustituir el polling de los agentes por una llamada bloqueante que retorna cuando ocurre una condición en GitHub.
2. **`get`**: lecturas de PR, issue, reviews, comentarios, checks/runs servidas desde caché compartida o con peticiones condicionales (ETag / `If-None-Match`, que con `304` no consumen cuota REST).
3. **Caché compartida entre todos los agentes de la misma máquina**, con deduplicación: si N agentes vigilan el mismo PR, se hace una sola consulta.
4. **Conmutación REST ↔ GraphQL**: para cada lectura, elegir la vía más barata o con más margen de cuota restante.
5. **Gobernador de cuota**: conocer el estado de cuota a partir de las cabeceras `X-RateLimit-*` ya recibidas (sin llamar a `/rate_limit`) y frenar/redistribuir *antes* de chocar, nunca con un 403.
6. **`quota`**: exponer ese estado a agentes sin gastar cuota.
7. **Escritura, control mínimo**: las escrituras que pasan por OctoGuard (a) invalidan la caché de lo que tocan y (b) se espacian para no disparar los límites secundarios de creación de contenido. Sin batching ni optimizaciones complejas.
8. **Autodescubrimiento al estilo `orca`**: el binario enseña a usarse a sí mismo (`agent-context`, guías embebidas) y la skill de cada agente es un arranque mínimo que lo invoca.
9. **Multi-agente**: skill en el estándar abierto de Agent Skills (`SKILL.md`), instalable para Claude Code, Codex, Gemini CLI y Copilot.
10. **Visibilidad**: `octoguard stats` muestra en consola, de forma gráfica, el uso de cuota y el ahorro conseguido.
11. **Distribución**: Homebrew (tap en la cuenta `Muriano`), binarios para macOS y Linux (arm64/amd64).

## 3. No-objetivos (v1)

- Optimizar los orquestadores como programas: se hace por otro lado. Solo nos importan los agentes que estos lanzan, y esos usan la skill.
- Ser un proxy transparente o reemplazar `gh`: los agentes usan `octoguard` a propósito porque la skill se lo indica.
- Batching o reordenación de escrituras, colas de mutaciones, reintentos inteligentes de escritura.
- Webhooks / servidor público / GitHub App. Todo es local.
- Compartir caché entre máquinas.
- Gestión de múltiples tokens o rotación de cuentas.

## 4. Anti-objetivos (sería un fracaso aunque "funcione")

- **Que un agente decida con datos viejos.** Ej.: mergear creyendo que los checks están en verde porque la caché lo decía. Escrituras y merges siempre validan en fresco.
- Que OctoGuard **añada** bloqueos: un fallo del demonio no puede dejar al agente sin poder trabajar (debe degradar a llamada directa con el token, y decirlo).
- Que la skill sea tan larga o tan ruidosa que el agente la ignore y vuelva al `sleep 30`.
- Filtrar el token: nunca en logs, caché, salida ni socket accesible por otros usuarios.

## 5. Restricciones

- Lenguaje: **Go**.
- Repo **público**: `github.com/Muriano/OctoGuard`. Licencia: decisión del builder (sugerido MIT o Apache-2.0).
- Instalación con `brew` (tap `Muriano/homebrew-tap`).
- Autenticación: token obtenido de `gh auth token` (con `GH_TOKEN` / `GITHUB_TOKEN` como prioridad si existen). Sin login propio.
- Funciona en local, multi-proceso, sin privilegios.
- Compatible con el estándar de Agent Skills (`SKILL.md` con frontmatter `name` / `description`).

## 6. Decisiones tomadas

Las marcadas **(usuario)** son del usuario; **(Claude)** son decisiones mías que el usuario no ha discutido explícitamente pero ha dado por buenas al aprobar el resumen.

- (usuario) Go o Rust → **Go** (Claude).
- (usuario) Instalación con Homebrew.
- (usuario) Repo público en la cuenta personal `Muriano`.
- (usuario) Alcance: los agentes la usan voluntariamente vía skill; no proxy transparente.
- (usuario) Mecanismos: REST↔GraphQL, ETag, microcaché compartida entre agentes de la máquina.
- (usuario) Patrón de autodescubrimiento tipo `orca`.
- (usuario) Comando de estadísticas gráfico en consola con uso y ahorro → nombre `stats` (Claude).
- (Claude) Escritura: solo invalidación de caché + espaciado anti-límite-secundario.
- (Claude) **Arquitectura demonio + CLI**: el CLI `octoguard` habla con un demonio local por socket Unix (permisos 0600, en el directorio de usuario). El demonio se autoarranca con el primer uso y se apaga tras un periodo de inactividad. El demonio es quien posee la caché, deduplica consultas en vuelo (single-flight) y agrupa `wait`s sobre el mismo recurso.
- (Claude) **Caché persistente en SQLite** (WAL) en el directorio de caché de usuario (`os.UserCacheDir()/octoguard`). Guarda cuerpo, ETag, `Last-Modified`, timestamp y vía usada (REST/GraphQL).
- (Claude) **Fallback**: si el demonio no arranca o no responde, el CLI hace la llamada directa (con ETag desde la caché si puede) y avisa por stderr.
- (Claude) Escrituras y merges: siempre en fresco; tras escribir, invalidan las claves afectadas.

### Decisiones añadidas tras la revisión del builder (Claude)

- **Semántica de checks.** `checks-done` significa que ningún check run ni commit status sigue en `queued`, `in_progress` o `pending`. `checks-green` significa que todos los checks **requeridos** por la protección de rama están en `success`, `neutral` o `skipped` y que ninguno ha terminado en `failure`, `cancelled` o `timed_out`. Si la rama no tiene checks requeridos, se exige todo lo anterior a todos los checks. Ambos incluyen check runs y commit statuses.
- **Merge.** `octoguard merge` lee en fresco y, si `checks-green` no se cumple, rechaza con un motivo claro y sugiere `octoguard wait … --until checks-green`. Si se cumple, delega en la API de merge de GitHub, que sigue aplicando la protección de rama. No hay `--force` en la v1.
- **Tokens y caché.** El CLI resuelve el token en cada invocación (`GH_TOKEN` → `GITHUB_TOKEN` → `gh auth token`) y lo pasa al demonio por el socket con cada petición. La caché, la deduplicación single-flight y el seguimiento de cuota se separan por `(host, hash del token)`, así que nunca se sirven datos entre tokens distintos. El demonio no guarda tokens en disco.
- **Host.** La v1 solo soporta github.com. Aun así, el host forma parte de las claves de caché y de cuota para no rehacer el esquema si más adelante se añade GitHub Enterprise.
- **Escrituras de la v1**, elegidas por volumen en la evidencia: `pr create`, `pr comment`, `pr edit`, `pr merge`, `issue comment`, `issue create`, `project item-add` y `project item-edit`. Todas invalidan sus claves y pasan por el espaciado anti-límite-secundario. Las lecturas de Projects quedan fuera de la v1.
- **Duración de `wait` frente a los timeouts de los agentes.** `wait` bloquea como máximo `--timeout`, que por defecto son 8 minutos, por debajo del límite de 10 minutos de la herramienta Bash de Claude Code. Al vencer devuelve `{"status":"pending", …}` con un código de salida propio (no es un error). La skill y la guía indican que se vuelva a llamar con el mismo comando. Mientras tanto el poller del demonio sigue activo durante un periodo de gracia, de modo que la siguiente llamada continúa sin coste extra.
- **Demonio caído.** El CLI intenta arrancarlo una vez. Si no lo consigue, `get` hace una llamada directa con el ETag de la caché en disco, y `wait` sondea dentro del propio proceso con un intervalo conservador (60 s como mínimo, con backoff según la cuota). En ambos casos avisa por stderr. Así no se bloquea al agente y tampoco se reintroduce el polling agresivo.

## 7. Superficie para el agente (contrato de comportamiento)

Nombres orientativos; la forma exacta de flags es del builder. Toda salida por defecto en JSON estable (para agentes), con `--format text` opcional.

| Comando | Qué hace |
|---|---|
| `octoguard wait pr <ref> --until <cond>` | Bloquea hasta que se cumple la condición o vence `--timeout`. Condiciones v1: `checks-done`, `checks-green`, `review`, `comment`, `merged`, `closed`, `changed` (cualquier cambio). Devuelve el estado final y qué cambió. |
| `octoguard wait issue <ref> --until <cond>` | Igual para issues: `comment`, `closed`, `labeled`, `changed`. |
| `octoguard wait run <ref>` | Espera a que termine un workflow run. |
| `octoguard get pr\|issue\|reviews\|comments\|checks\|runs <ref>` | Lectura servida desde caché/ETag. `--fresh` fuerza validación. `--max-age` permite tolerar antigüedad. |
| `octoguard quota` | Estado de cuota REST, GraphQL y search (de cabeceras vistas), y presupuesto restante estimado. Sin llamadas a la API salvo si nunca se ha visto ninguna cabecera. |
| `octoguard pr create\|comment\|edit\|merge`, `issue comment\|create`, `project item-add\|item-edit` | Escrituras de la v1 (ver decisiones tras revisión): invalidan caché y respetan el espaciado. Merge valida checks en fresco. |
| `octoguard agent-context` | Esquema de comandos legible por máquina (JSON), pensado para agentes. |
| `octoguard guide [tema]` | Guías Markdown embebidas en el binario, con la versión del binario. Temas mínimos: `quickstart`, `waiting`, `reading`, `quota`, `writes`, `anti-patterns` (no hacer `sleep`+`gh`, no `--watch`, no `gh api rate_limit`). |
| `octoguard skills install [--agent claude\|codex\|gemini\|copilot\|all]` / `skills update` | Instala/actualiza el `SKILL.md` de arranque en la ubicación que espera cada agente. |
| `octoguard daemon status\|stop` | Diagnóstico del demonio: vivo/no, versión, waiters activos. |
| `octoguard stats` | Panel **gráfico en consola** de uso y ahorro (ver §7.1). |

### 7.1 `octoguard stats` (petición del usuario)

Pensado para que **lo mire una persona**: gráfico, legible de un vistazo, en la terminal. No es solo un volcado de contadores.

- **Qué muestra:**
  - **Cuota actual** de REST, GraphQL y search, con barras de uso y la hora de reset.
  - **Ahorro en el periodo:** llamadas evitadas y cuota ahorrada (REST y puntos GraphQL), desglosadas por mecanismo: aciertos de caché, `304` por ETag, deduplicación entre agentes y `wait` frente a polling.
  - **Histórico:** sparkline o barras por hora/día de llamadas reales frente a llamadas evitadas, y los momentos en que el gobernador frenó.
  - **Rate limits:** límites evitados (frenazos del gobernador antes de agotar la cuota) y límites sufridos (403/429 reales, que deberían tender a 0).
  - **Top recursos y agentes**, si se pueden identificar (p. ej. por proceso padre o una etiqueta `--agent` opcional).
- **Opciones:** `--since 1h|24h|7d|30d` (por defecto 24h), `--live` para refrescar en vivo leyendo solo datos locales (sin gastar cuota) y `--format json` para scripts y agentes.
- **Datos:** los contadores se persisten en la SQLite local agregados por hora, para que el histórico sobreviva a reinicios del demonio. Nunca se envían fuera.
- **Cómo se calcula "ahorrado"** (debe ser honesto y figurar en la propia salida o en `guide stats`): acierto de caché o `304` = 1 llamada REST evitada, o el coste GraphQL de esa consulta. Deduplicación = (N waiters − 1) consultas por ciclo. `wait` = las consultas que habría hecho un bucle de referencia de `sleep 30` durante la espera, menos las que realmente hizo el demonio. El valor de referencia (30 s) se muestra y es configurable.
- **Librería visual:** la elige el builder (p. ej. Lip Gloss / Bubble Tea de Charm). La salida debe degradar bien sin color (`NO_COLOR`) o con un ancho de terminal estrecho.

`<ref>` acepta `owner/repo#N`, URL de GitHub, o `N` resolviendo el repo desde el directorio actual (git remote).

### La skill

`SKILL.md` corto: cuándo usarla (cualquier lectura o espera sobre GitHub), regla dura ("nunca `sleep`+`gh`, nunca `--watch`, nunca `gh api rate_limit`; usa `octoguard wait` / `get` / `quota`"), y "ejecuta `octoguard guide quickstart` para el resto". El contenido detallado vive en el binario para no desincronizarse.

## 8. Comportamiento interno (qué, no cómo)

- **Peticiones condicionales**: toda lectura REST cacheada se revalida con `If-None-Match`. Un `304` refresca el timestamp sin consumir cuota.
- **Elección de vía**: el demonio conoce el coste de cada lectura por vía (1 petición REST vs. coste en puntos GraphQL) y el margen restante de cada cupo. Política inicial sugerida: REST+ETag para revalidar recursos ya cacheados; GraphQL cuando una lectura compuesta en REST costaría varias peticiones (p. ej. PR + reviews + checks de golpe), o cuando el cupo REST va justo. Afinar es del builder.
- **Polling adaptativo dentro de `wait`**: intervalo según condición y presupuesto; un solo poller por recurso aunque haya N waiters; backoff cuando se acerca el límite. Opcional (builder): usar el endpoint de notificaciones con `X-Poll-Interval` / `Last-Modified` como señal barata de "algo cambió".
- **Gobernador**: umbrales de presupuesto que reducen frecuencia de polling antes de que la cuota se agote; respeta `Retry-After` y límites secundarios.
- **Invalidación**: escribir sobre un PR/issue invalida sus claves de caché (pr, reviews, comments, checks según corresponda).

## 9. Lo que queda al builder

- Librerías concretas (p. ej. `go-github`, `githubv4`, driver SQLite sin cgo), estructura de paquetes, framework de CLI.
- Esquema exacto de la tabla de caché, TTLs por tipo de recurso, umbrales del gobernador e intervalos de polling.
- Formato exacto de JSON de salida y de `agent-context` (debe ser estable y versionado).
- Ubicaciones exactas de instalación de skills por agente (verificar la convención vigente de cada uno).
- Mecanismo de autoarranque/apagado del demonio y bloqueo para que solo exista uno.
- CI, goreleaser, licencia, tests de integración (sugerido: servidor GitHub falso para tests de ETag/cuota).
- Cómo medir el éxito (sugerido: `octoguard stats` + volver a pasar el script de reconocimiento sobre transcripciones a las dos semanas de uso).

# Medir el éxito de OctoGuard

**Criterio de éxito:** cero paradas por rate limit en el uso diario multi-agente ([diseño](design/2026-11-11-octoguard-v1-design.md)). Se mide comparando el uso de `gh` de los agentes antes y después de dos semanas usando OctoGuard.

## Herramientas

### `scripts/recon/gh_recon.py`

Es el script de reconocimiento que se usó en el diseño. Recorre las transcripciones locales (`*.jsonl`) de Claude Code (`~/.claude/projects`) y de Codex (`~/.codex/sessions`) que se modificaron en los últimos N días, y cuenta lo siguiente por cada agente:

| Métrica | Qué cuenta |
|---|---|
| `gh_invocations` | Invocaciones `gh <subcomando>` dentro de los comandos de shell de los agentes |
| `sleep_gh` | Comandos que combinan `sleep` con `gh` (polling a mano) |
| `watch` | Comandos `gh … --watch` y `gh run watch` |
| `rate_limit_errors` | Líneas con `API rate limit exceeded` o `secondary rate limit` |

También lista los subcomandos más usados, con las rutas de `gh api` anonimizadas: solo se conservan las palabras conocidas de la API REST, los números pasan a `N` y el resto (owner, repo, usuario, rama, SHA…) pasa a `X`, por ejemplo `repos/X/X/pulls/N`. También lista las tools MCP de GitHub usadas.

```sh
python3 scripts/recon/gh_recon.py --days 14            # tabla
python3 scripts/recon/gh_recon.py --days 14 --json     # para guardar y comparar
```

El script es de solo lectura, no usa la red y solo depende de la librería estándar de Python 3. `--claude-dir` y `--codex-dir` cambian las rutas de las transcripciones.

### `octoguard stats`

`octoguard stats --since 14d` pinta el panel de uso y ahorro del periodo. Con `--format json` saca lo mismo en JSON para guardarlo. Incluye los rate limits evitados y los sufridos (ver `octoguard guide stats`).

## Baseline

Esta es la medición del diseño, sin OctoGuard: 14 días en Claude Code.

| Métrica | Valor |
|---|---|
| Llamadas `gh` | ~18.700 |
| Errores de rate limit | 261 |
| `--watch` | 293 |

Antes de instalar OctoGuard, guarda tu propio baseline con el mismo script. Así comparas siempre con el mismo método:

```sh
python3 scripts/recon/gh_recon.py --days 14 --json > recon-before.json
```

## Procedimiento

1. Guarda el baseline (`recon-before.json`) como se indica arriba.
2. Instala OctoGuard y la skill de arranque (`octoguard skills install --agent all`).
3. Trabaja con normalidad durante **2 semanas**, con varios agentes en paralelo.
4. Repite la medición:
   ```sh
   python3 scripts/recon/gh_recon.py --days 14 --json > recon-after.json
   octoguard stats --since 14d
   octoguard stats --since 14d --format json > stats-14d.json
   ```
5. Compara `recon-before.json` con `recon-after.json` y revisa el panel de `stats`.

## Criterio

- **Obligatorio:** 0 errores de rate limit sufridos. Tiene que cumplirse en `octoguard stats` (rate limits sufridos = 0) y también en `rate_limit_errors` del script.
- **Señales esperadas:** bajan `gh_invocations`, `sleep_gh` y `watch`, porque los agentes usan `octoguard get` y `octoguard wait`. En `stats` se ve ahorro por caché, por `304` y por `wait` frente a polling.

## Limitaciones

- La ventana se calcula por la fecha de modificación de cada transcripción. Una sesión larga que empezó antes de la ventana cuenta entera.
- `rate_limit_errors` es una heurística por línea. También cuenta las menciones del texto del error (por ejemplo, una conversación que habla de él o una issue que lo cita). Por eso hay que comparar siempre con el mismo script, y la referencia es el valor de `stats`.
- Las cifras dependen de la ventana: volver a pasar el script sobre otros 14 días no reproduce exactamente el baseline del diseño.

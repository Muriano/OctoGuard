# OctoGuard

Guardián de cuota de la API de GitHub para agentes de IA (Claude Code, Codex, Gemini, Copilot…).

Los agentes que trabajan en paralelo con el mismo token agotan la cuota de GitHub una y otra vez: polling con `sleep 30`, `gh pr checks --watch`, lecturas repetidas del mismo PR… OctoGuard les da un binario (`octoguard`) y una skill de arranque para que **lean y esperen** recursos de GitHub gastando la mínima cuota:

- `octoguard wait`: una llamada bloqueante que sustituye al bucle de polling.
- Peticiones condicionales con ETag (`304` no consume cuota).
- Microcaché compartida entre todos los agentes de la máquina, con deduplicación.
- Conmutación REST ↔ GraphQL según coste y margen.
- Gobernador de cuota que frena antes de chocar.
- `octoguard stats`: panel en consola con el uso y el ahorro.
- Autodescubrimiento para agentes (`octoguard agent-context`, `octoguard guide`).

## Instalación

Con Homebrew (macOS y Linux):

```sh
brew install muriano/tap/octoguard
```

También hay binarios para darwin y linux (amd64 y arm64) en [Releases](https://github.com/Muriano/OctoGuard/releases), con `checksums.txt`. O con Go:

```sh
go install github.com/Muriano/OctoGuard/cmd/octoguard@latest
```

## Uso

```sh
octoguard --version
```

Los comandos (`get`, `wait`, `quota`, `stats`, `guide`…) se describen en el [diseño](docs/design/2026-11-11-octoguard-v1-design.md) y llegan con sus issues.

## Publicar una versión

Requiere el secret `HOMEBREW_TAP_TOKEN` en el repo: un token fine-grained con permiso *Contents: Read and write* solo sobre `Muriano/homebrew-tap`.

```sh
git tag vX.Y.Z
git push origin vX.Y.Z
```

El workflow `release.yml` ejecuta los tests y goreleaser. Publica los binarios y los checksums en la release de GitHub y actualiza la fórmula en `Muriano/homebrew-tap`.

> Estado: **diseño**. Aún no hay código. Ver [diseño](docs/design/2026-11-11-octoguard-v1-design.md), [plan](docs/plans/2026-11-11-octoguard-v1.md) y los [issues](https://github.com/Muriano/OctoGuard/issues).

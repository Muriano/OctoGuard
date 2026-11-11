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

> Estado: **en desarrollo**. Ya está el esqueleto (CLI raíz, CI y servidor GitHub falso para tests); los comandos llegan con los siguientes [issues](https://github.com/Muriano/OctoGuard/issues). Ver [diseño](docs/design/2026-11-11-octoguard-v1-design.md) y [plan](docs/plans/2026-11-11-octoguard-v1.md).

## Desarrollo

Requiere Go 1.23+ y [golangci-lint](https://golangci-lint.run/) v2.

```sh
make build   # bin/octoguard, con la versión de `git describe`
make test    # go vet + go test -race (incluye testdata/fakegh)
make lint    # golangci-lint
```

## Licencia

[MIT](LICENSE)

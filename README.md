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

> Estado: **diseño**. Aún no hay código. Ver [diseño](docs/design/2026-11-11-octoguard-v1-design.md), [plan](docs/plans/2026-11-11-octoguard-v1.md) y los [issues](https://github.com/Muriano/OctoGuard/issues).

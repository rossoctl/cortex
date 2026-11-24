# Optional components

Kustomize components that add to the central stack. None is built yet.

| Component | Adds | Status |
|---|---|---|
| `events/` | ClickHouse (StatefulSet + PVC), a schema Job, and a `logs` pipeline in the collector for the accountant's structural events (rossoctl/accountant PR #86, `docs/events/events.md`) | next PR |

An overlay takes a component with `components: [../../components/<name>]`. The collector's config
stays one file in one generated ConfigMap, so a component can add its pipeline as a second file and a
second `--config` argument.

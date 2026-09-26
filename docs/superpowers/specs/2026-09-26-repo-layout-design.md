# Repository layout and naming

Status: **implemented** · 2026-09-26 · the top-level layout and `core/`'s package names

Two counts in this document were wrong when written, and are corrected here rather than
silently: there are **seven** `replace` directives naming the library, not five (§6's
table missed the two that sit inside `replace (` blocks), and the theme shares measured
on the finished branch are Framework 57.1% / Cost 21.1% / Observability 10.0% / **Auth
1.6%** against 53,323 non-test lines — close enough to §1's figures to leave the
argument intact, but not the same numbers.

Implementation also turned up eight distinct classes of reference that a substitution
sweep cannot see, against the one class §6 anticipated. They are enumerated in the PR
description; the short version is that §6's "verify by shape, not by pattern" was the
right instruction and still understated the number of shapes.

## 1. Problem

The flatten (#1134) removed the vestigial `authbridge/` level. That fixed the depth
and exposed two things it had been hiding.

**The root does not describe the product.** Ten directories, and four of them are
implementation satellites rather than things a reader goes looking for:
`lineage-attach`, `proxy-init`, `sparc-service`, and `storage` — the last holding a
single driver whose interface lives elsewhere.

**The names are one product generation behind, and one of them is wrong on a second
axis.** `authlib` is not merely stale from the AuthBridge→Cortex rename; measured by
non-test lines it is **1.7% auth**:

| Theme | Lines | Share |
|---|---|---|
| Framework — `pipeline`, `plugins`, `listener`, `config`, `spiffe` | 32,984 | 65% |
| Cost — `pricing`, `costing`, `costledger`, `costevent`, `usage` | 11,288 | 22% |
| Observability — `session`, `sessionapi`, `observe`, `redact` | 5,368 | 10% |
| **Auth** — `auth`, `validation`, `bypass`, `exchange`, `contracts` | **882** | **1.7%** |

The docs compound it: they call it *"the shared auth library"* and enumerate it as
*"validation, exchange, cache, bypass, spiffe, routing, auth, config…"* — the 1.7%
listed first, the 32% omitted.

Inside it, 29 top-level packages have the same flat-and-wide shape the root had,
including five siblings on one subject and two pairs of confusingly similar names.

## 2. Scope

**In:** the top-level regrouping, the `authlib` module rename, and internal package
renames within it.

**Out, deliberately:** every published contract. This change breaks nothing that
another component consumes by name:

| Untouched | Why |
|---|---|
| Image names `authbridge`, `-envoy`, `-lite`, `-cpex` | the operator selects images **by name** |
| Binary names `authbridge-proxy`, `abctl`, … | released artifacts, on users' PATH |
| `x-authbridge-{direction,secret,unmapped}` | wire protocol; injected by Envoy config in the rossoctl Helm chart |
| `AUTHBRIDGE_*` (9 env vars) | `install.sh` accepts them; users may set them |
| `authbridge-config`, `authbridge-runtime{,-config,-mtls}` | operator creates and mounts these |
| `/etc/authbridge/config.yaml` | operator's volume spec |
| `authproxy-routes` | operator contract, older naming stratum |
| `abph_` credential-handle prefix | on the wire |
| The published install URL and `install.sh:485`'s `--ref` probe | #1134's compatibility surface |

Retiring those is a separate project needing a deprecation window and coordinated
PRs in at least two other repositories.

## 3. Target layout

```
cortex/
├── core/                    ← authlib
│   ├── pipeline/  plugins/  listener/  config/
│   ├── cost/                ← pricing, costing, costledger, costevent, usage
│   ├── session/  sessionapi/  observe/  redact/
│   ├── auth/  validation/  bypass/
│   ├── storage/
│   │   └── redis/           ← storage/redis, beside the interface it implements
│   ├── memstore/            ← shared
│   ├── bootstrap/           ← runtimeutil
│   ├── capabilities/        ← contracts
│   ├── tlsconfig/           ← tls
│   └── tlsbridge/  spiffe/  praxis/  …
├── cmd/                     unchanged
├── deploy/                  ← proxy-init, sparc-service, lineage-attach
├── demos/  docs/  tests/    unchanged
└── scripts/
    ├── dev/                 ← the three loose shell scripts
    ├── hooks/  profile-tags/  readme-demo/
```

Top level: **10 directories → 7.**

### Why `deploy/` is not the mistake `authbridge/` was

`authbridge/` grouped *everything*, so it separated nothing. `deploy/` groups only
things that run in a cluster beside the sidecar — two published images and the
tooling that attaches lineage to a pod — and leaves the library, the binaries, the
docs and the demos visible at root. The root then reads as the product.

### Why `storage/redis` moves under `core/`

It imports `authlib/storage` and registers a `Factory` for its `Store` interface. The
interface is in the library; the implementation was a sibling of the library. It stays
its own Go module, so its Redis dependency remains isolated — nesting a module inside
another module's directory is legal and already the case for `demos/*`.

## 4. Package renames, and why each

Each name is taken from what the package's own doc comment says it does.

| From | To | The package's own description |
|---|---|---|
| `authlib` | `core` | 1.7% auth; the module every binary and both external consumers import |
| `shared` | `memstore` | "a generic, process-scoped, TTL key→value store… intentionally semantics-free". `memstore` also contrasts with `storage`, which is the cross-pod persistent one |
| `runtimeutil` | `bootstrap` | "process-level helpers shared by the binaries… logging setup, the SIGUSR1 toggle, the health and stats servers" — process startup, not a "util" |
| `contracts` | `capabilities` | "defines **capability interfaces** that protocol extensions implement" — its own words |
| `tls` | `tlsconfig` | builds `*crypto/tls.Config` values. `tlsbridge` forges certificates. Two unrelated jobs, near-identical names |
| `pricing` `costing` `costledger` `costevent` `usage` | `cost/{pricing,settle,ledger,event,usage}` | five siblings on one subject. `costing` → `settle` because it "turns one response's facts into one settled cost" |

### Deliberately not renamed

- **`placeholder`** — reads like a TODO, but it is accurately named: it "defines the
  convention for opaque credential handles." Renaming it would also divorce the
  package from the `abph_` wire prefix, which cannot change. Left alone on purpose.
- **`praxis`** — an external product's name, correctly used for the converter that
  targets it.
- **`clientstate`, `tlsbridge`, `spiffe`, `pipeline`, `plugins`, `listener`** — accurate.

## 5. "Do not break anything", as testable requirements

The binding constraint. Each line is a gate, not an intention:

1. No image name changes — the six `image_config` entries in `build.yaml`
   (`proxy-init`, `authbridge`, `authbridge-envoy`, `authbridge-lite`,
   `authbridge-cpex`, `sparc-service`) byte-identical. Grep for `- name:` alone
   returns 15 and catches step names too; assert on the image list.
2. No binary name changes — `cmd/` subdirectory names byte-identical.
3. No wire/env/ConfigMap/mount-path changes — the §2 table verified by grep at final HEAD.
4. `install.sh` untouched, including line 485's `--ref` probe.
5. `go mod tidy -diff` clean in **all 12** modules. Moving `storage/redis` under
   `core/` adds none — it is already one of the twelve.
6. Every module builds, vets, and tests; `authlib`'s successor passes `-race`.
7. All five build profiles resolve, and the four pure-Go ones compile: `local`,
   `full`, `lite`, `envoy`. `cpex` needs cgo and a pinned `libcpex_ffi.a`, so it is
   tag-resolution only here — `build.yaml` covers it via the image build.
8. `install_test.sh` 113/113; `pytest tests/` 12/12.
9. A real `docker build` of the proxy image **and** of `proxy-init` and
   `sparc-service`, whose build contexts move.
10. `readme-demo`'s staleness test passes — it walks the repo by relative path.
11. Every markdown link resolves, checked by **resolving targets on disk** with
    `#anchor` stripped. Not by reading the diff: #1134 lost a review round to links
    whose text never changed while the file moved beneath them.
12. No stale path anywhere, verified by the enumeration in §6.

## 6. The bare-name class — the enumeration this change must not repeat

#1134's substitutions matched `authbridge/` **with a trailing slash**, so sixteen
bare-directory spellings survived, nine of them `cd authbridge` lines whose blocks
then ran `pip install` or `kubectl apply` — procedures that failed at step 1.

`authlib` has the same exposure. Nineteen sites, counted:

| Shape | Count |
|---|---|
| `cd authlib` | 7 |
| `COPY authlib` (Dockerfiles) | 4 |
| `go-version-file: authlib/go.mod` | 4 |
| `working-directory: authlib` | 1 |
| `go -C authlib` | 1 |
| `use ./authlib` (`go.work`) | 1 |
| `directory: /authlib` (dependabot) | 1 |

Substitutions must therefore match `authlib` **without** requiring a following
slash, and the sweep must be verified by shape — raw mentions, then path-segment
reads, then each spelling — rather than by enumerating patterns.

Unlike #1134, the `replace` directives **do** change: `authlib` → `core` keeps the
same depth, so `../../authlib => ../../core` needs both sides edited. Five of them.

The fixed-depth class from #1134 is **absent**: nothing changes depth except
`storage/redis`, which gains one level. Its own paths must be checked for that.

## 7. Risks

| Risk | Mitigation |
|---|---|
| A bare-name spelling survives | §6's enumeration; sweep by shape, then grep at final HEAD |
| A published contract changes by accident | §5.1–5.4 are grep gates over the §2 table |
| `core/storage/redis` breaks as a nested module | `go mod tidy -diff` + build in all 12 modules |
| Moved build contexts break an image | real `docker build` of all three affected images |
| `readme-demo` writes outside the repo | it walks relative paths; §5.10 runs its staleness test |
| Downstream consumers | already break on their next bump from #1134; this adds `authlib`→`core` to the same one-time import sweep. Both are pinned; neither breaks on merge |
| One large PR | the owner asked for one. Mitigated by commit-per-concern so review can proceed piecewise |

## 8. Commit structure

One PR, six commits, each independently reviewable:

1. `deploy/` — move `proxy-init`, `sparc-service`, `lineage-attach`; fix build contexts.
2. `scripts/dev/` — move the three loose shell scripts; fix references.
3. `authlib` → `core` — module path, 850 imports, 19 bare names, 5 `replace` directives.
4. `storage/redis` → `core/storage/redis` — module path plus its own relative paths.
5. Package renames — `memstore`, `bootstrap`, `capabilities`, `tlsconfig`, `cost/*`.
6. Prose — rewrite the descriptions that call it "the shared auth library" and
   enumerate the 1.7% while omitting the 32%.

**As implemented: twelve commits, not six.** The six above all landed as planned; the
other six are fallout each of which deserved its own reviewable boundary rather than
being folded into the commit that caused it — retargeting ten relative links the
`deploy/` move stranded, making the migration script actually reproduce its own output,
restoring the gofmt ordering the rename disturbed, and correcting the godoc headers and
filenames the package renames left behind. That the fallout matched the planned work
commit-for-commit is the honest measure of how much of this change was invisible to
the sweep that performed it.

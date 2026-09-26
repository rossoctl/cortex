# AuthBridge praxis-sidecar combined image — the authbridge-praxis config
# generator plus a Praxis proxy binary, in a single container.
#
# How this differs from the other AuthBridge images: authbridge-praxis is not a
# proxy. It converts an AuthBridge config into a Praxis config (and, when the
# inbound pipeline declares jwt-validation, a Praxis policy document), and
# Praxis is the data plane. The entrypoint therefore runs the generator to
# completion first, then execs Praxis against the generated config. See
# entrypoint.sh for the ordering guarantees.
#
# The Praxis binary is built with `--features policy-engine`, which is OFF in
# Praxis's default feature set. That feature is what compiles in the `policy`
# filter; without it Praxis rejects the generated config outright with
# "unknown filter type: 'policy'" whenever a policy document was produced.
# Since the generator emits that filter for jwt-validation, the two must be
# built together — a default-feature Praxis in this image would fail to start
# on exactly the configs that need inbound auth.
#
# Build context: . (repository root; needs access to core/, storage/, and
# cmd/authbridge-praxis/)
#
# Praxis source is not vendored in this repo, so the Rust stage fetches it from
# git. PRAXIS_REPO / PRAXIS_REF pin what gets built; PRAXIS_REF defaults to a
# commit SHA rather than a tag or branch, so an image rebuild is reproducible
# even if the tag is moved or deleted upstream. Point PRAXIS_REPO at a fork to
# build that instead.

# ── Stage 1: Build the Praxis proxy binary (with the policy engine) ─────────
#
# Mirrors the recipe in Praxis's own Containerfile: rust:1.96-alpine (matching
# its rust-toolchain.toml) plus the musl / OpenSSL / cmake toolchain its
# dependency tree needs. OPENSSL_STATIC links OpenSSL into the binary so the
# runtime stage needs no OpenSSL package.
#
# Deliberately NOT digest-pinned, unlike the golang and alpine images below. A
# digest names one architecture's manifest, so pinning here would break
# multi-arch builds of this image — and this is the one stage that must build on
# both arm64 and amd64 from source. The tag is a minor-version tag
# (rust:1.96-alpine, not :latest or :1), so the toolchain version is still
# fixed; what floats is only the patch/base-image refresh, which is what keeps
# the multi-arch manifest usable. The build's reproducibility guarantee comes
# from PRAXIS_REF below, which pins the source actually being compiled.
#
# The runtime stages ARE digest-pinned: they ship the final artifacts, so
# supply-chain pinning matters more there than cross-arch flexibility.
FROM rust:1.96-alpine AS praxis-builder

ENV OPENSSL_STATIC=1

RUN apk add --no-cache musl-dev openssl-dev openssl-libs-static pkgconf cmake make g++ git

ARG PRAXIS_REPO=https://github.com/praxis-proxy/praxis.git
# An immutable commit SHA, not a tag or branch. A branch would make rebuilds
# non-reproducible and could silently pick up a schema change the generator in
# stage 2 does not emit for; a tag is better but still mutable — it can be
# force-moved or deleted upstream, and a build that silently follows it is the
# same hazard one step removed. The SHA below is release v0.5.3, which carries
# the policy-engine feature and the `policy` filter fields this image's
# generated configs use.
#
# To bump: resolve the new tag to its commit and update both the SHA and the
# comment together, so the human-readable version never drifts from what builds.
#   git ls-remote --tags https://github.com/praxis-proxy/praxis.git v0.5.4
#
# PRAXIS_REF = v0.5.3
ARG PRAXIS_REF=7c6cef76a9b8449c3617dacfa597ee6d7ac9d82e

WORKDIR /praxis

# Fetch exactly PRAXIS_REF, or fail.
#
# Two shapes are accepted — a tag or a bare SHA / branch — and each is checked
# out from the ref it just fetched. That coupling is the point: an earlier
# version fell back to fetching ALL branch tips and checking out FETCH_HEAD, so
# a transient failure fetching the pinned ref would silently build main instead
# — defeating the reproducibility the PRAXIS_REF comment above promises, on
# exactly the condition it was meant to guarantee. Failing the build is the only
# safe outcome; a stage that quietly compiles different source than it was told
# to is worse than one that stops.
#
# The default PRAXIS_REF is a SHA, which takes the second form: fetching an
# arbitrary commit by SHA needs the server to permit it (GitHub allows this for
# public repos, but not universally), and the tag form is kept first so a
# tag-valued override still works.
RUN git init -q . \
    && git remote add origin "${PRAXIS_REPO}" \
    && ( ( git fetch -q --depth 1 origin "refs/tags/${PRAXIS_REF}:refs/tags/${PRAXIS_REF}" \
           && git checkout -q "refs/tags/${PRAXIS_REF}" ) \
         || ( git fetch -q --depth 1 origin "${PRAXIS_REF}" \
              && git checkout -q FETCH_HEAD ) \
         || { echo "FATAL: could not fetch PRAXIS_REF=${PRAXIS_REF} from ${PRAXIS_REPO}" >&2; exit 1; } ) \
    && git --no-pager log --oneline -1

# `policy-engine` pulls in the praxis-policy crates (identity/jwt, Cedar PDP,
# OAuth delegator). Build only the server crate; the workspace's test and
# benchmark members are not needed for the binary.
#
# --locked builds against upstream's committed Cargo.lock and fails if that file
# would need to change. Without it cargo is free to resolve newer semver-
# compatible dependency versions than the ones upstream tested and released,
# so two builds of the same PRAXIS_REF could ship different dependency trees —
# and a compromised or simply broken point release of any transitive crate would
# be pulled in silently. Pinning the source commit without pinning the lockfile
# leaves most of the dependency surface unpinned.
RUN cargo build --release --locked -p praxis-proxy --features policy-engine \
    && cp target/release/praxis /praxis-bin \
    && strip /praxis-bin

# Fail the build here rather than at container start if the policy-engine
# feature did not take effect: a Praxis without the `policy` filter cannot run
# the configs this image generates, and that failure would otherwise surface as
# a crashlooping pod.
#
# The assertion is POSITIVE — a complete, valid policy document plus a config
# that references it must make `--validate` exit 0. An earlier version instead
# probed with a deliberately-missing config_path and passed when the output did
# NOT contain "unknown filter type". That inverted form fails open on precisely
# the condition it guards: the string is an upstream `format!` with no stability
# guarantee, and PRAXIS_REF is overridable, so any rewording of that message
# would make the gate pass unconditionally — silently shipping a Praxis that
# rejects every generated config. Exit status cannot drift the same way.
#
# The probe policy mirrors what BuildPolicy emits (an identity/jwt plugin over
# trusted_issuers), so this also catches an upstream change to the policy schema
# that the generator has not been updated for. HS256 with an inline secret keeps
# it self-contained: no JWKS fetch, so no network dependency at build time.
RUN printf 'plugins:\n\
  - name: probe-jwt\n\
    kind: identity/jwt\n\
    hooks: [identity.resolve]\n\
    mode: sequential\n\
    priority: 10\n\
    on_error: fail\n\
    config:\n\
      header: Authorization\n\
      claim_mapper: standard\n\
      trusted_issuers:\n\
        - issuer: "https://probe.invalid/realms/probe"\n\
          audiences: ["probe-aud"]\n\
          algorithms: ["HS256"]\n\
          decoding_key:\n\
            kind: secret\n\
            secret: "build-time-probe-secret-not-used-at-runtime"\n\
          leeway_seconds: 60\n' > /probe-policy.yaml \
    && printf 'listeners:\n\
  - name: probe\n\
    address: "127.0.0.1:18099"\n\
    filter_chains: [c]\n\
filter_chains:\n\
  - name: c\n\
    filters:\n\
      - filter: policy\n\
        config_path: /probe-policy.yaml\n\
        require_protocol_metadata: false\n\
      - filter: static_response\n\
        status: 200\n' > /probe.yaml \
    && if ! /praxis-bin --validate --config /probe.yaml; then \
         echo "FATAL: praxis rejected a policy-filter config — it was probably built WITHOUT" >&2; \
         echo "       --features policy-engine, or the policy schema has changed upstream." >&2; \
         exit 1; \
       fi \
    && echo "policy-engine verified: policy filter accepted a valid policy document" \
    && rm -f /probe.yaml /probe-policy.yaml

# ── Stage 2: Build the authbridge-praxis config generator ───────────────────
#
# `-ldflags="-s -w"` drops the symbol table and DWARF debug info to shave ~30%
# off the binary. Go's runtime keeps `pclntab` separately, so panic stack
# traces still show function names.
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS go-builder

RUN apk add --no-cache git

WORKDIR /app

COPY core/ core/
COPY storage/ storage/
COPY cmd/authbridge-praxis/ cmd/authbridge-praxis/

ARG GO_BUILD_TAGS=""
ENV GOWORK=off
RUN cd cmd/authbridge-praxis && CGO_ENABLED=0 GOOS=linux go build -tags "${GO_BUILD_TAGS}" -ldflags="-s -w" -o /authbridge-praxis .

# ── Stage 3: Runtime ───────────────────────────────────────────────────────
#
# alpine (has bash for the entrypoint, ~5 MB base). authbridge-praxis is
# CGO_ENABLED=0 static; the Praxis binary is musl-linked against the same libc
# family as this base, with OpenSSL statically linked in.
FROM alpine:3.20@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d

RUN apk add --no-cache bash ca-certificates

COPY --from=go-builder --chmod=755 /authbridge-praxis /usr/local/bin/authbridge-praxis
COPY --from=praxis-builder --chmod=755 /praxis-bin /usr/local/bin/praxis
COPY --chmod=755 cmd/authbridge-praxis/entrypoint.sh /usr/local/bin/entrypoint.sh

# The generator writes into /tmp and Praxis reads from there, so the runtime
# user needs a writable /tmp. Alpine ships it 1777, which covers an arbitrary
# UID; created explicitly so the contract is visible rather than inherited.
RUN mkdir -p /tmp && chmod 1777 /tmp

USER 1001

# 8080 reverse proxy / 8081 forward proxy — the listener addresses the
# generator derives from the AuthBridge config. 9091 is the Praxis admin
# endpoint (/healthy, /ready, /metrics); the generator binds it on
# AuthBridge's health port so an existing /readyz probe keeps working against
# Praxis's /ready.
EXPOSE 8080 8081 9091

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

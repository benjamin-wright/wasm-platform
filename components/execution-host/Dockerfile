# syntax=docker/dockerfile:1
# Build context: components/execution-host/
# Named build context: --build-context workspace=<repo-root>
#
# The primary context contains only this component's source. The workspace
# named context supplies Cargo manifests, proto definitions, and the WIT file;
# BuildKit transfers only the matched files. Minimal stub sources are generated
# so that `cargo metadata` can resolve every workspace member.
#
# Requires BuildKit (default in Docker >= 23.0).

# ── Stage 1: cargo-chef installer ─────────────────────────────────────────────
# Pinned to the minimum Rust version required by wasmtime (rust-version in Cargo.toml).
FROM lukemathwalker/cargo-chef:latest-rust-1.91-slim-bookworm AS chef
WORKDIR /build

# ── Stage 2: dependency planner ───────────────────────────────────────────────
# Only manifest files, proto definitions, and the WIT file are copied from the
# workspace named context. Stub sources satisfy `cargo metadata` without pulling
# in sibling source trees. No compilation happens here.
FROM chef AS planner
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler && rm -rf /var/lib/apt/lists/*
COPY --from=workspace Cargo.toml Cargo.lock ./
COPY --parents --from=workspace components/*/Cargo.toml examples/*/Cargo.toml ./
RUN find components examples -maxdepth 2 -name Cargo.toml -exec dirname {} \; \
    | xargs -I{} sh -c 'mkdir -p {}/src && touch {}/src/lib.rs {}/src/main.rs'
COPY --parents --from=workspace proto/configsync/v1/configsync.proto ./
COPY --from=workspace framework/runtime.wit ./framework/runtime.wit
COPY . ./components/execution-host/
RUN cargo chef prepare --recipe-path recipe.json

# ── Stage 3: binary builder ───────────────────────────────────────────────────
# Cooks all dependency crates before copying application source. Source-only
# changes will cache-hit at the `cargo chef cook` layer.
FROM chef AS builder
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler && rm -rf /var/lib/apt/lists/*
COPY --from=planner /build/recipe.json recipe.json
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=cargo-git,target=/usr/local/cargo/git \
    --mount=type=cache,id=execution-host-target,target=/build/target \
    cargo chef cook --release --recipe-path recipe.json
COPY --from=workspace Cargo.toml Cargo.lock ./
COPY --parents --from=workspace components/*/Cargo.toml examples/*/Cargo.toml ./
RUN find components examples -maxdepth 2 -name Cargo.toml -exec dirname {} \; \
    | xargs -I{} sh -c 'mkdir -p {}/src && touch {}/src/lib.rs {}/src/main.rs'
COPY --parents --from=workspace proto/configsync/v1/configsync.proto ./
COPY --from=workspace framework/runtime.wit ./framework/runtime.wit
COPY . ./components/execution-host/
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=cargo-git,target=/usr/local/cargo/git \
    --mount=type=cache,id=execution-host-target,target=/build/target \
    cargo build --release --bin execution-host && \
    cp target/release/execution-host /build/execution-host

# ── Stage 4: runtime ──────────────────────────────────────────────────────────
# distroless/cc-debian12 provides glibc and libgcc_s, both required at runtime:
# glibc for the standard Rust target, libgcc_s for stack unwinding. Switch to
# distroless/base-debian12 only if `ldd execution-host` confirms no libgcc dep.
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=builder /build/execution-host /usr/local/bin/execution-host
EXPOSE 3000
CMD ["/usr/local/bin/execution-host"]

# syntax=docker/dockerfile:1
# Build context: components/gateway/
# Named build context: --build-context workspace=<repo-root>
#
# The primary context contains only this component's source. The workspace
# named context supplies Cargo manifests and proto definitions; BuildKit
# transfers only the matched files. Minimal stub sources are generated so that
# `cargo metadata` can resolve every workspace member.
#
# Requires BuildKit (default in Docker >= 23.0).

# ── Stage 1: cargo-chef installer ─────────────────────────────────────────────
FROM lukemathwalker/cargo-chef:latest-rust-1.91-slim-bookworm AS chef
WORKDIR /build

# ── Stage 2: dependency planner ───────────────────────────────────────────────
FROM chef AS planner
# protobuf-compiler is needed by tonic_build in build.rs.
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler && rm -rf /var/lib/apt/lists/*
COPY --from=workspace Cargo.toml Cargo.lock ./
COPY --parents --from=workspace components/*/Cargo.toml examples/*/Cargo.toml ./
RUN find components examples -maxdepth 2 -name Cargo.toml -exec dirname {} \; \
    | xargs -I{} sh -c 'mkdir -p {}/src && touch {}/src/lib.rs {}/src/main.rs'
COPY --parents --from=workspace proto/gateway/v1/gateway.proto ./
COPY . ./components/gateway/
RUN cargo chef prepare --recipe-path recipe.json

# ── Stage 3: binary builder ───────────────────────────────────────────────────
FROM chef AS builder
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler && rm -rf /var/lib/apt/lists/*
COPY --from=planner /build/recipe.json recipe.json
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=cargo-git,target=/usr/local/cargo/git \
    --mount=type=cache,id=gateway-target,target=/build/target \
    cargo chef cook --release --recipe-path recipe.json
COPY --from=workspace Cargo.toml Cargo.lock ./
COPY --parents --from=workspace components/*/Cargo.toml examples/*/Cargo.toml ./
RUN find components examples -maxdepth 2 -name Cargo.toml -exec dirname {} \; \
    | xargs -I{} sh -c 'mkdir -p {}/src && touch {}/src/lib.rs {}/src/main.rs'
COPY --parents --from=workspace proto/gateway/v1/gateway.proto ./
COPY . ./components/gateway/
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=cargo-git,target=/usr/local/cargo/git \
    --mount=type=cache,id=gateway-target,target=/build/target \
    cargo build --release --bin gateway && \
    cp target/release/gateway /build/gateway

# ── Stage 4: runtime ──────────────────────────────────────────────────────────
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=builder /build/gateway /usr/local/bin/gateway
EXPOSE 3000
CMD ["/usr/local/bin/gateway"]

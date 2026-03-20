# Build context: repo root.
# Named build context: --build-context wasm=<dir-containing-hello_world.wasm>
#
# Requires BuildKit (default in Docker >= 23.0).

# ── Stage 1: cargo-chef installer ─────────────────────────────────────────────
# Pinned to the minimum Rust version required by wasmtime (rust-version in Cargo.toml).
FROM rust:1.91-slim-bookworm AS chef
RUN cargo install cargo-chef --locked
WORKDIR /build

# ── Stage 2: dependency planner ───────────────────────────────────────────────
# Copies the full workspace so cargo-chef can resolve every member and produce
# a complete recipe. No compilation happens here.
FROM chef AS planner
COPY . .
RUN cargo chef prepare --recipe-path recipe.json

# ── Stage 3: binary builder ───────────────────────────────────────────────────
# Cooks all dependency crates before copying application source. Source-only
# changes will cache-hit at the `cargo chef cook` layer.
FROM chef AS builder
COPY --from=planner /build/recipe.json recipe.json
RUN cargo chef cook --release --recipe-path recipe.json
COPY . .
RUN cargo build --release --bin execution-host --bin precompile

# ── Stage 4: AOT compiler ─────────────────────────────────────────────────────
# Compiles the WASM component to a native .cwasm artifact using the same Engine
# configuration as the runtime binary, ensuring load-time compatibility.
#
# The `wasm` named build context (--build-context wasm=...) must contain
# hello_world.wasm. The output is architecture-specific; multi-platform builds
# must run this stage once per target architecture.
FROM builder AS aot
COPY --from=wasm . /build/wasm/
RUN /build/target/release/precompile \
        /build/wasm/hello_world.wasm \
        /build/hello_world.cwasm

# ── Stage 5: runtime ──────────────────────────────────────────────────────────
# distroless/cc-debian12 provides glibc and libgcc_s, both required at runtime:
# glibc for the standard Rust target, libgcc_s for stack unwinding. Switch to
# distroless/base-debian12 only if `ldd execution-host` confirms no libgcc dep.
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=builder /build/target/release/execution-host /usr/local/bin/execution-host
COPY --from=aot /build/hello_world.cwasm /opt/wasm/hello_world.cwasm
ENV WASM_MODULE_PATH=/opt/wasm/hello_world.cwasm
EXPOSE 3000
CMD ["/usr/local/bin/execution-host"]

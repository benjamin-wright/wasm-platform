# Build context: repo root.
#
# Requires BuildKit (default in Docker >= 23.0).

# ── Stage 1: cargo-chef installer ─────────────────────────────────────────────
# Pinned to the minimum Rust version declared in Cargo.toml.
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
RUN cargo build --release --bin module-cache

# ── Stage 4: runtime ──────────────────────────────────────────────────────────
# distroless/cc-debian12 provides glibc and libgcc_s required by the standard
# Rust target. Switch to distroless/static-debian12 only if `ldd module-cache`
# confirms no dynamic library dependencies.
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=builder /build/target/release/module-cache /usr/local/bin/module-cache
EXPOSE 3000
CMD ["/usr/local/bin/module-cache"]

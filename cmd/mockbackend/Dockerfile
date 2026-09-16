# The mock backend, containerised so ASS can start and stop it like a real one.
# We build the static binary on the host (matching Go toolchain) and just drop it
# onto a slim base — no in-image Go toolchain, no surprise downloads. See
# scripts/build-mockbackend.sh.
FROM debian:bookworm-slim
COPY mockbackend /usr/local/bin/mockbackend
# Default; ASS overrides ADDR/PORT/VERB/JOB_MS per service at create time.
ENV PORT=8000
ENTRYPOINT ["/usr/local/bin/mockbackend"]

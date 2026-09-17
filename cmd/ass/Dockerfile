# ASS, containerised. Multi-stage: build a static binary, drop it on a slim base.
#
# The final image is tiny and has NO Go toolchain and NO GPU libraries — because
# ASS never opens a CUDA context. It only bosses the host Docker daemon around
# (over the mounted socket) and reaches backends over the host network. All the
# heavy torch/CUDA stuff lives inside the *backend* images ASS starts, never here.
# (See the Deployment topology note in CLAUDE.md.)
FROM golang:1.27-bookworm AS build
WORKDIR /src
# Cache modules first, then build. modernc.org/sqlite is pure Go, so CGO off
# yields a genuinely static binary with no libc surprises on the slim base.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/ass ./cmd/ass

FROM debian:bookworm-slim
WORKDIR /app
COPY --from=build /out/ass /usr/local/bin/ass
# Config is mounted in at run time; data (sqlite + harvested artifacts) lives on a
# mounted volume. Both are relative to this workdir — see docker-compose.yml.
ENTRYPOINT ["ass"]
CMD ["-config", "ass.toml"]

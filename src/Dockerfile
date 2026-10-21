# syntax=docker/dockerfile:1

# --- builder: static app binary + static goose, no CGO ---
FROM golang:1.25 AS builder
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux

# goose CLI as a pinned binary. no_sqlite3 removes the only CGO driver
# (we need only postgres) -> the build is purely static.
RUN GOBIN=/out go install -tags='no_sqlite3' -ldflags='-s -w' \
        github.com/pressly/goose/v3/cmd/goose@v3.27.1

# dependencies in a separate layer for the cache
COPY go.mod go.sum ./
RUN go mod download

# the application. timetzdata embeds the IANA timezone database into the binary: distroless/static
# does not contain /usr/share/zoneinfo, while main.go calls time.LoadLocation (MYMEET_TIMEZONE).
COPY . .
RUN go build -trimpath -tags='timetzdata' -ldflags='-s -w' -o /out/transcrib ./cmd/transcrib

# --- final: distroless static, nonroot ---
FROM gcr.io/distroless/static:nonroot
ARG VCS_REF=unknown
LABEL org.opencontainers.image.revision=$VCS_REF
COPY --from=builder /out/transcrib /app
COPY --from=builder /out/goose /goose
COPY --from=builder /src/migrations /migrations
# CMD (not ENTRYPOINT): the migrate service replaces the command with /goose entirely.
CMD ["/app"]

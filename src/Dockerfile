# syntax=docker/dockerfile:1

# --- builder: статический бинарь приложения + статический goose, без CGO ---
FROM golang:1.25 AS builder
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux

# goose CLI как pinned-бинарь. no_sqlite3 убирает единственный CGO-драйвер
# (нам нужен только postgres) -> сборка чисто статическая.
RUN GOBIN=/out go install -tags='no_sqlite3' -ldflags='-s -w' \
        github.com/pressly/goose/v3/cmd/goose@v3.27.1

# зависимости отдельным слоем для кеша
COPY go.mod go.sum ./
RUN go mod download

# приложение. timetzdata встраивает базу таймзон IANA в бинарь: distroless/static
# не содержит /usr/share/zoneinfo, а main.go зовёт time.LoadLocation (MYMEET_TIMEZONE).
COPY . .
RUN go build -trimpath -tags='timetzdata' -ldflags='-s -w' -o /out/transcrib ./cmd/transcrib

# --- final: distroless static, nonroot ---
FROM gcr.io/distroless/static:nonroot
ARG VCS_REF=unknown
LABEL org.opencontainers.image.revision=$VCS_REF
COPY --from=builder /out/transcrib /app
COPY --from=builder /out/goose /goose
COPY --from=builder /src/migrations /migrations
# CMD (не ENTRYPOINT): migrate-сервис целиком подменяет команду на /goose.
CMD ["/app"]

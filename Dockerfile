# syntax=docker/dockerfile:1

# --- Build stage -------------------------------------------------------------
# Generated templ (*_templ.go) and compiled Tailwind (web/static/css/styles.css)
# are committed, so the image build only needs the Go toolchain.
FROM golang:1.26 AS build

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is not needed (pgx is pure Go).
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/app \
 && go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/scripts/migrate

# --- Runtime stage -----------------------------------------------------------
# ffmpeg is required to remux inbound WhatsApp voice notes (audio/ogg; opus)
# into a format browsers can play.
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --create-home --uid 10001 app

WORKDIR /app

COPY --from=build /out/api ./bin/api
COPY --from=build /out/migrate ./bin/migrate

USER app

# Render injects PORT; config.Load() reads it.
ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["./bin/api"]

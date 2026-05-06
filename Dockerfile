# --- build stage ---
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

# --- runtime stage ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 bot
WORKDIR /app
COPY --from=build /out/bot /app/bot
COPY config.example.yaml /app/config.yaml
RUN mkdir -p /data && chown -R bot:bot /app /data
USER bot

ENV DB_PATH=/data/bot.db
VOLUME ["/data"]

ENTRYPOINT ["/app/bot", "--config", "/app/config.yaml"]

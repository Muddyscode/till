# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tilld ./cmd/tilld

FROM alpine:3.20
# tzdata: reports and logs use Africa/Lagos. ca-certificates: TLS to managed Postgres.
RUN apk add --no-cache tzdata ca-certificates \
    && addgroup -S -g 10001 till && adduser -S -u 10001 -G till till
ENV TZ=Africa/Lagos TILL_ENV=production TILL_HTTP_ADDR=:8080
COPY --from=build /out/tilld /usr/local/bin/tilld
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s CMD wget -qO- http://127.0.0.1:8080/livez >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/tilld"]

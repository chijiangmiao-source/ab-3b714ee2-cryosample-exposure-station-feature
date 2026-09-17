# ---- build ----
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /bin/server ./cmd/server

# ---- run ----
FROM alpine:3.21
RUN adduser -D -u 10001 app && mkdir -p /data && chown app:app /data
COPY --from=build /bin/server /bin/server
USER app
ENV PORT=8080 \
    DB_PATH=/data/app.db \
    GIN_MODE=release
EXPOSE 8080
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=12 \
  CMD wget -qO- http://127.0.0.1:8080/api/health || exit 1
CMD ["/bin/server"]

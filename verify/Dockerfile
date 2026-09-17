# ---- build ----
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /bin/verify .

# ---- run ----
FROM alpine:3.21
COPY --from=build /bin/verify /bin/verify
CMD ["/bin/verify"]

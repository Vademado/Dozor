FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/guard-admin ./cmd/guard

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -S dozor \
    && adduser -S -G dozor dozor \
    && mkdir /data \
    && chown dozor:dozor /data
COPY --from=build /out/guard-admin /usr/local/bin/guard-admin
USER dozor:dozor
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/guard-admin"]
CMD ["-listen", "0.0.0.0:8080", "-data", "/data"]

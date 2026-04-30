FROM golang:1.23-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags="-s -w" -o /out/proxyharbor ./cmd/proxyharbor

FROM alpine:3.20
RUN addgroup -g 65532 proxyharbor \
    && adduser -D -H -u 65532 -G proxyharbor proxyharbor \
    && apk add --no-cache ca-certificates \
    && mkdir -p /var/lib/proxyharbor \
    && chown -R proxyharbor:proxyharbor /var/lib/proxyharbor
USER 65532:65532
COPY --from=build /out/proxyharbor /usr/local/bin/proxyharbor
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/proxyharbor"]

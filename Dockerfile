FROM golang:1.23-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags="-s -w" -o /out/proxyharbor ./cmd/proxyharbor

FROM alpine:3.20
RUN adduser -D -H proxyharbor && apk add --no-cache ca-certificates
USER proxyharbor
COPY --from=build /out/proxyharbor /usr/local/bin/proxyharbor
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/proxyharbor"]

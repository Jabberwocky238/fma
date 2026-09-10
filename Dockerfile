# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION
ARG COMMIT=unknown
ARG RELEASE_TIME
RUN build_version="${VERSION:-dev-$(date -u +%Y%m%dT%H%M%SZ)}"; \
    build_time="${RELEASE_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -trimpath \
    -ldflags "-s -w -X main.version=$build_version -X main.commit=$COMMIT -X main.releaseTime=$build_time" \
    -o /out/fma .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=build /out/fma /usr/local/bin/fma
USER 65532:65532
EXPOSE 2525 1587 1465 1110 1995 1143 1993 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/fma"]
CMD ["-domain", "example.com", "-smtp", "0.0.0.0:2525", "-submission", "0.0.0.0:1587", "-smtps", "0.0.0.0:1465", "-pop3", "0.0.0.0:1110", "-pop3s", "0.0.0.0:1995", "-imap", "0.0.0.0:1143", "-imaps", "0.0.0.0:1993", "-http", "0.0.0.0:8080"]

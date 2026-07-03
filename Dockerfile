# Stage 1 (Build)
FROM golang:1.24.11-alpine AS builder

ARG VERSION
# RUSTIC_VERSION and RUSTIC_TARGET control the rustic binary bundled for the
# deduplicated/encrypted backup adapter. Override RUSTIC_TARGET for non-amd64
# nodes (e.g. aarch64-unknown-linux-musl).
ARG RUSTIC_VERSION=0.11.3
ARG RUSTIC_TARGET=x86_64-unknown-linux-musl
RUN apk add --update --no-cache git make mailcap curl tar
# Extract into a scratch dir then move the binary out — the archive layout
# (binary at root vs. nested in a versioned dir) varies between releases.
RUN mkdir -p /tmp/rustic \
    && curl -fsSL "https://github.com/rustic-rs/rustic/releases/download/v${RUSTIC_VERSION}/rustic-v${RUSTIC_VERSION}-${RUSTIC_TARGET}.tar.gz" \
       | tar -xz -C /tmp/rustic \
    && mv "$(find /tmp/rustic -type f -name rustic | head -n1)" /usr/local/bin/rustic \
    && chmod +x /usr/local/bin/rustic \
    && rm -rf /tmp/rustic \
    && /usr/local/bin/rustic --version
WORKDIR /app/
COPY go.mod go.sum /app/
RUN go mod download
COPY . /app/
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/realmctl/wings/system.Version=$VERSION" \
    -v \
    -trimpath \
    -o wings \
    wings.go
RUN echo "ID=\"distroless\"" > /etc/os-release

# Stage 2 (Final)
FROM gcr.io/distroless/static:latest
COPY --from=builder /etc/os-release /etc/os-release
COPY --from=builder /etc/mime.types /etc/mime.types

COPY --from=builder /app/wings /usr/bin/
# rustic binary powering the deduplicated/encrypted backup adapter.
COPY --from=builder /usr/local/bin/rustic /usr/bin/

ENTRYPOINT ["/usr/bin/wings"]
CMD ["--config", "/etc/realm/config.yml"]

EXPOSE 8080 2022

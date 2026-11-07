# Built by goreleaser (see .goreleaser.yaml `dockers`): the build context holds
# the prebuilt linux binaries plus configs/, so this file does no compiling.
# Local build: goreleaser release --snapshot --clean
FROM gcr.io/distroless/static:nonroot

WORKDIR /app
# Binaries and configs/ share a directory: config.FindDataFile looks in
# <exe dir>/configs for the bundled config.example.yaml and catalog.json.
COPY free-llm-proxy free-llm-scan /app/
COPY configs /app/configs

# Usage/reliability state and scanned catalogs go to $HOME/.free-llm-proxy-router
# (HOME=/home/nonroot, writable). Mount a volume there to keep them.
USER nonroot:nonroot
EXPOSE 8080
# The binary defaults to 127.0.0.1; inside a container that would be unreachable
# through -p, so the image listens on all interfaces. Publish the port only on
# localhost (-p 127.0.0.1:8080:8080) or set proxy.auth_token.
ENTRYPOINT ["/app/free-llm-proxy", "--host", "0.0.0.0"]

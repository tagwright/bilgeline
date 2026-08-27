# Build from the bilgeline repository root (single context):
#
#   docker build -t ghcr.io/tagwright/bilgeline:dev .
#
# core and beacon are consumed as published modules
# (github.com/tagwright/core, github.com/tagwright/beacon). GOPRIVATE makes the
# build fetch tagwright's own modules directly from their source rather than
# through the public module proxy. go.sum still verifies their integrity.

FROM golang:1.25 AS build

ENV GOPRIVATE=github.com/tagwright/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Static, VCS-stamp-free build. The version is stamped from the VERSION file
# into main.version, matching what `bilgeline version` prints.
RUN CGO_ENABLED=0 go build -buildvcs=false -ldflags "-s -w -X main.version=$(cat VERSION)" -o /out/bilgeline ./cmd/bilgeline

# distroless static:nonroot is a minimal, non-root base with ca-certificates and
# tzdata and nothing else: no shell, no package manager. bilgeline is a static
# CGO-free binary that only needs the container socket and the config volume, so
# this is all the surface it needs.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/bilgeline /usr/local/bin/bilgeline

ENTRYPOINT ["/usr/local/bin/bilgeline"]
CMD ["daemon"]

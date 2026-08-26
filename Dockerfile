# Build context note (read before you build):
#
# bilgeline's go.mod carries `replace github.com/tagwright/core => ../core`
# because core is not published yet. The build therefore needs BOTH the core/
# and bilgeline/ module trees in the build context, so the context is the PARENT
# directory that holds them as siblings, not the bilgeline/ dir itself:
#
#   docker build -f bilgeline/Dockerfile -t ghcr.io/tagwright/bilgeline:dev .
#
# run from the parent of core/ and bilgeline/ (the tagwright monorepo root).
#
# beacon is a different story: it is published and public
# (github.com/tagwright/beacon), so it is fetched over the network like any
# other module. GOPRIVATE points go at tagwright's own source for direct
# fetches rather than the public proxy; go.sum still verifies integrity.
#
# ONCE core is published and tagged, this simplifies to a single-context build,
# exactly like ballast did after beacon was published: drop the `replace` line
# from go.mod, drop the core COPY below, set the context back to bilgeline/, and
# restore the ballast-style `COPY go.mod go.sum ./ && go mod download && COPY .`
# layering for a better dependency cache.

FROM golang:1.25 AS build

# Fetch tagwright's own modules (beacon) directly from source, not the proxy.
ENV GOPRIVATE=github.com/tagwright/*

WORKDIR /src

# core/ must sit beside bilgeline/ so the `replace => ../core` resolves. Copy
# core first: it changes far less often than bilgeline, so it caches well.
COPY core/ ./core/
COPY bilgeline/ ./bilgeline/

WORKDIR /src/bilgeline

# Download the network deps (beacon and the transitive set) up front. core is a
# local replace, so nothing is fetched for it.
RUN go mod download

# Static, VCS-stamp-free build. The version is stamped from the VERSION file
# into main.version, matching what `bilgeline version` prints.
RUN CGO_ENABLED=0 go build -buildvcs=false \
    -ldflags "-s -w -X main.version=$(cat VERSION)" \
    -o /out/bilgeline ./cmd/bilgeline

# distroless static:nonroot is a minimal, non-root base with ca-certificates and
# tzdata and nothing else: no shell, no package manager. bilgeline is a static
# CGO-free binary that only needs the container socket and the config volume, so
# this is all the surface it needs.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/bilgeline /usr/local/bin/bilgeline

ENTRYPOINT ["/usr/local/bin/bilgeline"]
CMD ["daemon"]

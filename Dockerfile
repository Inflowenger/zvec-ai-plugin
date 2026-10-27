# The plugin is built without cgo, so the build stage needs no C toolchain —
# only the Go one. The zvec engine is a prebuilt shared library fetched in the
# same stage and copied beside the binary, where the plugin finds it on its own.
FROM golang:1.27-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/zvec-plugin . \
	&& ./scripts/fetch-zvec-lib.sh -d /out/lib

# Debian rather than Alpine: the engine's default build links glibc. For an
# Alpine image, pass `-l musl` to the fetch above and base this stage on alpine.
FROM gcr.io/distroless/base-debian12

WORKDIR /app
COPY --from=build /out/zvec-plugin /app/zvec-plugin
COPY --from=build /out/lib /app/lib

# Collections live here. Mount a volume: a plugin that loses this directory
# loses its vectors, and the data directory named in the settings profile has
# to be a path inside the container.
VOLUME ["/data"]

# PLUGIN_ID, INFRA_CRED and INFRA_URL come from the environment or a mounted
# .env.inflow. No zvec configuration belongs here — the data directory is named
# by the settings profile.
ENTRYPOINT ["/app/zvec-plugin"]

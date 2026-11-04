# Build stage: compile on the builder's own CPU and cross-compile for the target,
# so building an ARM image on an x86 CI runner needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off: a fully static binary that runs on an image with no C library.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /hirewire ./cmd/hirewire

# Run stage: no shell, no package manager, CA certificates for HTTPS, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /hirewire /hirewire
ENV LOG_FORMAT=json
ENTRYPOINT ["/hirewire"]
CMD ["run"]

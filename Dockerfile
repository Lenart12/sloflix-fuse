# Go cross-compiles on the build host; the runtime stage only copies, so multi-platform builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /sloflixfs .

FROM alpine:3
COPY --from=build /sloflixfs /usr/local/bin/
ENV SLOFLIX_MOUNT=/mnt/sloflix/library SLOFLIX_CACHE=/cache SLOFLIX_ALLOW_OTHER=true
CMD ["sloflixfs"]

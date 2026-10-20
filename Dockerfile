# Minimal Actlet image: a static, CGO-free binary on a distroless base,
# running as a non-root user.
#
#   make image                      # or:
#   docker build -t actlet:dev .
#
# Mount a volume at the session folder to keep sessions across container
# replacement; the container's own filesystem only survives a restart.

ARG GO_VERSION=1.27

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=unknown DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/actlet ./cmd/actlet \
 && mkdir /out/session

# distroless/static: no shell or package manager, but CA certificates,
# time zone data and a non-root user (65532).
FROM gcr.io/distroless/static-debian12:nonroot
ARG SESSION_DIR=/var/lib/actlet
COPY --from=build /out/actlet /usr/local/bin/actlet
COPY --from=build --chown=65532:65532 /out/session ${SESSION_DIR}
WORKDIR ${SESSION_DIR}
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/actlet"]
CMD ["help"]

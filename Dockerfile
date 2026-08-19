FROM golang:1.27-alpine AS build
WORKDIR /src

# Dependencies are their own layer: they change far less often than the code.
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# CGO off and a trimmed path: the binary has to run on a distroless image and
# must not leak the build machine's directory layout into panic traces.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]

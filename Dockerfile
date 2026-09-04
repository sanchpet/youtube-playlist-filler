# Build with the toolchain the repo pins, so the image and a local `go build` agree.
FROM golang:1.26.5-alpine AS build

WORKDIR /src
# Dependencies first: they change far less often than the code, so this layer survives most builds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static, stripped, and reproducible enough to compare: nothing here needs cgo, so there is no
# reason to carry a libc into the runtime image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/filler ./cmd/filler

# cmd/enroll is deliberately not built into this image. It exists to open a browser and record one
# human consent, and the cluster has neither — shipping it would only put an interactive OAuth flow
# next to the credential it produces.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/filler /filler
USER nonroot:nonroot
ENTRYPOINT ["/filler"]

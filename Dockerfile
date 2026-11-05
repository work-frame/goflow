# GoFlow's container image (Phase 11): the API, with the worker binary
# beside it. Build: docker build -t goflow .   Run: see compose.yaml.
#
# Two stages. The first has Go and the source and compiles. The second
# gets only the two binaries: no compiler, no source, no shell, no package
# manager. Whatever isn't in the image can't be exploited or leak.
#
# No secrets here, ever: no ARG or ENV holding a URL, password or token.
# Every instruction is saved in the image (docker history shows it), and
# Render passes each environment variable as a build argument, so a secret
# referenced here would be baked in. Settings come from the environment
# when the container starts.

FROM golang:1.27-trixie AS build
WORKDIR /src

# Modules first: this layer is reused until go.mod or go.sum change, so
# editing code doesn't download every module again.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# CGO_ENABLED=0: a fully static binary. The golang image has a C compiler,
# so without this Go links against the system's C library (for DNS, among
# others), and the final image has none: the container would fail to start
# with "exec /usr/local/bin/api: no such file or directory" (case 43).
# -trimpath keeps this machine's paths out of the binary; -s -w drops the
# debug tables (panics still show file and line).
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# distroless "static": CA certificates (for verify-full to Neon), time
# zone data, and a user called nonroot. About 2 MiB before the binaries.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/api /out/worker /usr/local/bin/
# Not root: a bug in GoFlow can't change anything the image owns.
USER nonroot:nonroot
# Documentation only. The API listens on GOFLOW_ADDR, else PORT (Render
# sets it), else :8080.
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/api"]

# pocketbox engine

The container engine behind PocketBox: an OCI image store, Dockerfile builds, a
container lifecycle and the Docker Engine API subset the app talks to.

```
pull ──▶ content store ──▶ layer apply ──▶ image rootfs
                                            │
                       run / build / compose ┘
```

Built for arm64 phones first: the engine runs inside the app's embedded Linux
guest, where each command is one program image. On a Linux or macOS host it
also runs directly and containers are plain processes.

## Use

```sh
pocketbox pull alpine:3.20
pocketbox run -d --name web -p 8080:80 nginx:alpine
pocketbox ps -a
pocketbox logs web
pocketbox build -t myapp:dev .
pocketbox serve --listen tcp://0.0.0.0:2375    # Docker API
```

`DOCKER_HOST=tcp://<host>:2375 docker ps` works against `serve`: ping, version,
info, containers (list/create/start/stop/restart/kill/wait/logs), images
(list/pull/delete), volumes.

## iOS app

`PocketBox/` is the SwiftUI app (XcodeGen `project.yml`, sources under
`PocketBox/Sources/PocketBox`): container list with start/stop/logs, image pull,
console, and an engine tab. It runs the engine inside an arm64 Linux guest; see
`runtime/README.md` for what is proven and what still needs a macOS linker.
`scripts/package-app.sh` assembles the app bundle with the runtime and engine
binaries and refuses to ship a non-Mach-O or a non-static-ELF binary.

## Layout

| package | role |
|---|---|
| `pkg/digest` | OCI content digests and verification |
| `pkg/oci` | manifest/index/config types, platform selection |
| `pkg/reference` | image name parsing and normalisation |
| `pkg/registry` | registry v2 client: token auth, manifests, blobs, uploads |
| `pkg/content` | content-addressable blob store |
| `pkg/layers` | layer application (whiteouts, opaque dirs, links, devices) |
| `pkg/images` | resolve/pull, image metadata, rootfs staging |
| `pkg/engine` | state, lifecycle, volumes, commit, Dockerfile builds |
| `pkg/buildkitclient` | Dockerfile parser/lowering and layer authoring |
| `pkg/api` | Docker Engine API subset |
| `pkg/netg` | host network bridge for sandboxes without sockets |
| `pkg/native` | process runner for host builds |
| `pkg/guestrunner` | container runner for the guest runtime |

## Build

```sh
go build ./cmd/pocketbox
go test ./...
POCKETBOX_PROBE=1 go test ./pkg/images -run Pull   # hits Docker Hub
CGO_ENABLED=0 go build -tags pocketbox_guest ./cmd/pocketbox
```

The `pocketbox_guest` tag routes image downloads through `pkg/netg`'s host
bridge instead of opening sockets in the sandbox.

## Compatibility notes

* arm64 images are selected from manifest lists automatically.
* Dockerfile support covers FROM, ARG, ENV, RUN, COPY, ADD (local), WORKDIR,
  USER, EXPOSE, VOLUME, LABEL, ENTRYPOINT, CMD, SHELL, STOPSIGNAL, ONBUILD.
  Multi-stage builds, `COPY --from`, `RUN --mount`, remote `ADD` and
  `FROM scratch` fail with a clear error rather than building something else.
* zstd layers are reported as unsupported (gzip and uncompressed work).

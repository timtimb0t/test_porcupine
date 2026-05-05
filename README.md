# Porcupine checker container

This directory contains a small Go-based Porcupine linearizability checker used by
SC consistency/schema-change tests.

The checker reads a JSON-lines history from `stdin`, checks a per-key integer
register model, and writes a JSON result to `stdout`. On failure, it can also
write debug artifacts such as the original history, the checker result, and a
Porcupine HTML visualization.

The container image is used so tests do not require a Go toolchain installed on
the host.

## Files

```text
Dockerfile
go.mod
go.sum
main.go
```

The final runtime image is based on `scratch` and contains only the statically
linked `porcupine_checker` binary.

## Build

From this directory:

```bash
podman build -t localhost/porcupine-checker:dev .
```

or with Docker:

```bash
docker build -t localhost/porcupine-checker:dev .
```

The `localhost/porcupine-checker:dev` tag is for local development. On CI, the
image is pulled from a registry. The image name is configured via the
`PORCUPINE_CHECKER_IMAGE` environment variable (used by the Python wrapper):

```python
CHECKER_IMAGE = os.environ.get("PORCUPINE_CHECKER_IMAGE", "localhost/porcupine-checker:dev")
```

To use a different image (e.g. from CI registry), set this variable:

```bash
PORCUPINE_CHECKER_IMAGE=ghcr.io/scylladb/porcupine-checker:latest pytest ...
```

## Build for another architecture

The Dockerfile accepts `TARGETARCH`. When using `podman build --platform` or
`docker buildx`, the `TARGETARCH` argument is set automatically. You can also
pass it explicitly.

Example for ARM64:

```bash
podman build \
  --build-arg TARGETARCH=arm64 \
  -t localhost/porcupine-checker:dev .
```

The default is:

```text
TARGETARCH=amd64
```

## Reproducing a failed check

If a test fails and artifacts were written, rerun the checker on the saved
history:

```bash
podman run --rm -i \
  -v /path/to/porcupine-checker-output:/output:Z \
  localhost/porcupine-checker:dev \
  --output-dir /output \
  < /path/to/porcupine-checker-output/history.jsonl
```

Then open the generated visualization:

```text
/path/to/porcupine-checker-output/porcupine_viz_key_<key>.html
```


## Making changes to the checker

1. Edit `main.go` (or add new `.go` files).

2. Test locally:

   ```bash
   go build -o porcupine_checker .
   ./porcupine_checker < history.jsonl
   ```

3. Rebuild the container image:

   ```bash
   podman build -t localhost/porcupine-checker:dev .
   ```

4. Run the test against the new image:

   ```bash
   PORCUPINE_CHECKER_IMAGE=localhost/porcupine-checker:dev pytest ...
   ```

5. When ready, push the image to the registry and update the default tag
   if needed.

## Notes

The Dockerfile uses a multi-stage build:

1. A `golang:<version>-trixie` builder image downloads dependencies and builds
   the checker.
2. A minimal `scratch` runtime image receives only the final static binary.

The binary is built with:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH}
```

This keeps the runtime image small and avoids requiring libc or other runtime
packages inside the final container.

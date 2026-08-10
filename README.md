# filecosystem-workers

Consumes transformation jobs published by
[`filecosystem-be`](../filecosystem-be), processes the file with libvips and
reports the outcome back over RabbitMQ.

A worker is stateless: it reads the source from object storage, writes the
result next to it, and publishes a `job.result` event. It never touches
Postgres, so scaling out means starting more replicas.

## Requirements

- Go 1.25+
- libvips 8.10+ (`brew install vips` on macOS, `apt install libvips-dev` on Debian)
- The infrastructure from `filecosystem-be` (`make infra-up` there)

cgo is required because libvips is a C library. The Makefile adds the Homebrew
`pkg-config` path automatically on macOS.

## Getting started

```bash
cp .env.example .env
make run
```

Or build the container image, which bundles libvips:

```bash
make docker-build
```

## How a job flows

1. A message arrives on the `image.jobs` queue.
2. The source object is downloaded from Amazon S3.
3. libvips applies the operation, honouring EXIF orientation first.
4. The encoded result is uploaded under `results/{job_id}/`.
5. A `job.result` event goes out on the `filecosystem.events` exchange.

Failures are reported as events too, rather than being retried silently, so the
user sees a real error instead of a job that never finishes.

## Configuration

| Variable             | Default | Purpose                                     |
| -------------------- | ------- | ------------------------------------------- |
| `WORKER_CONCURRENCY` | CPU cores | Jobs processed in parallel, also the prefetch |
| `MAX_SOURCE_BYTES`   | 50 MiB  | Largest source file a worker will decode     |
| `JOB_TIMEOUT`        | 2m      | Deadline for a single job                    |
| `METRICS_ADDR`       | `:8090` | Serves `/healthz` and `/readyz`              |

See `.env.example` for the full list, including the broker and storage
credentials.

## Layout

```
cmd/worker              entrypoint and wiring
internal/config         environment configuration
internal/contracts      queue message schema, mirrored in filecosystem-be
internal/processor/image libvips operations
internal/queue          RabbitMQ client with reconnect
internal/storage        Amazon S3 object storage
internal/worker         job orchestration and result reporting
```

`internal/contracts/contracts.go` is duplicated byte-for-byte in the API
repository. Change both together or the queue protocol breaks.

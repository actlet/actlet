# Performance

What Actlet costs, measured rather than promised. There are no targets: resource limits are the implementor's, set on the container, and these numbers say what to expect inside them. `make bench` measures all of it on the machine it runs on, and `make bench-linux` does the same in a Linux container.

## Where the numbers come from

Measured on 2026-10-21 at commit `dfd2f30`, on two setups:

- **macOS**: an Apple M1 Max (10 cores), macOS on APFS.
- **Linux**: the `golang:1.27` image in Docker Desktop's Linux VM (arm64) on the same machine. Its disk is the VM's, so `fsync` there costs what the VM makes it cost, not what bare-metal storage would.

| What | Measured by |
|---|---|
| Binary sizes, startup, idle memory | `test/bench/process.go`, against the built binaries: `actlet run` against `actlet-devgw` with a demo task, five runs |
| Journal append and recovery | `BenchmarkAppendDurable`, `BenchmarkAppendNoSync`, `BenchmarkSync`, `BenchmarkRecover` and `BenchmarkCompressionRatio` in `internal/journal`, on records shaped like an agent's |
| Activity throughput | `BenchmarkThroughput` in `internal/activity`: events emitted while a sender reads and acknowledges them, as the gateway connection does |
| Attach fan-out | `BenchmarkHubFanout` in `internal/attach`: events published to 1, 8 and 32 viewers reading as fast as they can |

## The binaries and the process

| | macOS | Linux |
|---|---|---|
| `actlet` | 15.1 MiB | 14.7 MiB |
| `actlet-cli` | 14.0 MiB | 13.6 MiB |
| `actlet-devgw` | 12.9 MiB | 12.6 MiB |
| `actlet run`, start to registered with the gateway, median of 5 (min–max) | 19.3 ms (19.1–273.9) | 4.2 ms (4.2–26.3) |
| Resident memory of an idle session, median of 5 | 17.1 MiB | 21.1 MiB |

The binaries are static, stripped and CGO-free. Startup includes opening the session folder and the journal, connecting and registering. The slow first run on each setup is a cold start: binary and libraries not yet in the page cache. Idle memory is a session waiting between steps of its task, with the gateway connected; the agent's tools are separate processes and come on top.

## The journal

Appending, with each batch synced before its records are acknowledged:

| Records per batch | macOS: records/s | Linux: records/s | Stored size against raw |
|---|---|---|---|
| 1 | 212 | 3,274 | 1 : 2.2 |
| 8 | 1,576 | 17,932 | 1 : 3.0 |
| 64 | 9,961 | 55,685 | 1 : 3.4 |
| 256 | 31,598 | 94,642 | 1 : 3.6 |

A sync costs 4.5 ms on macOS, where it is `F_FULLFSYNC` and reaches the disk, and 0.25 ms in the Linux VM. Durable appends cost one sync per batch, so throughput grows with the batch: concurrent writers, such as parallel tool calls, share a sync. Without syncing, encoding, compressing and writing reach about 60,000 single records/s and 120,000 in batches of 64 on both setups.

Recovery, opening a journal of 60,000 records: 24.7 MiB on disk, 85.2 MiB before compression.

| | macOS | Linux |
|---|---|---|
| Checking framing and checksums, as `Open` does | 5.9 ms (4.4 GB/s) | 6.6 ms (3.9 GB/s) |
| Decoding every record, as a reader does | 131 ms | 159 ms |

zstd's default level stores agent-like records at about 1 : 3.7 in batches of 64. The fastest level is within 7% of that; the better level gains 4% for about a third more time.

## Activity

| | macOS | Linux |
|---|---|---|
| Emitted, sent and acknowledged, in memory | 382 ns per event (2.6 million/s) | 638 ns (1.6 million/s) |
| The same through the spill file, with a 16-event queue | 124 µs per event (8,100/s) | 8.3 µs (120,000/s) |

Emitting never waits: these are the rates at which a sender keeps up. Spilling happens only while the gateway is unreachable or slow. It writes each spilled event as a batch of its own, which is cheap on Linux but slow on macOS's filesystem. Both are far above what a session emits: the demo session, a busy agent with three subagents, emits 81 events in about two minutes.

## Attach fan-out

| Viewers | macOS: per event published | Linux: per event published | Skipped deliveries (macOS / Linux) |
|---|---|---|---|
| 1 | 145 ns | 317 ns | 21% / 87% |
| 8 | 1.9 µs | 2.0 µs | 2% / 47% |
| 32 | 13.0 µs | 6.8 µs | 0% / 14% |

Publishing is never held up by viewers. A viewer that can't keep up skips events and is told how many, rather than slowing the session down. In this benchmark events come millions of times a second, far faster than any session makes them, so viewers skip. That shows the mechanism, not something a real session meets: at a session's rate, every viewer gets every event.

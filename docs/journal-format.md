# Session journal format

The session journal is the file Actlet uses to recover a session after a pause, a stop or a crash. It is the authoritative record of execution progress, so it has to stay readable after the process dies at any moment, and it must never claim that something is saved when it isn't.

This document describes the on-disk format, what "durable" means for it, how a damaged journal is recovered, and what the format does **not** protect against.

> **Status: experimental.** This is format version 0. The layout, the record schema and the guarantees below may change incompatibly, and version 0 journals may not be readable by later versions. The provisional file extension is `.actlet`.

The implementation lives in `internal/journal`. Run `actlet-cli journal inspect <file>` to look inside a journal and `actlet-cli journal export --jsonl <file>` to dump its records.

## Design in brief

- **Append-only.** Records are only ever added at the end of the file. Nothing already written is modified in place. The only rewrite is when recovery cuts off an incomplete final batch.
- **Batches.** Records are grouped into batches. Each batch is encoded and compressed on its own, checksummed, and closed by a commit trailer. A reader can validate and decode any batch without the ones before it, apart from the sequence-number check.
- **One writer.** A single goroutine owns the file and writes batches in order. Any number of goroutines can submit records to it. An exclusive file lock keeps other processes out.
- **Acknowledge after sync.** `Append` returns only after the batch holding the record has been written *and* synced to stable storage.
- **Bounded reading.** Readers check every length against configured limits before allocating or reading anything, and they hold at most one batch in memory.

## File layout

All integers are little-endian.

```
file   := header batch*
batch  := batch-header payload checksum trailer
```

### File header

| Offset | Size | Field | Notes |
|---:|---:|---|---|
| 0 | 8 | magic | `89 41 43 54 4C 45 54 0A` (`\x89ACTLET\n`) |
| 8 | 2 | format version | `0` |
| 10 | 2 | flags | must be `0` |
| 12 | 2 | session ID length *n* | 1–255 |
| 14 | *n* | session ID | UTF-8 |
| 14+*n* | 4 | header CRC | CRC-32C of bytes `0 .. 14+n` |

The non-ASCII first byte and the trailing newline make it easy to spot a file that went through a text-mode transfer, or a file that isn't a journal at all.

### Batch

| Offset | Size | Field | Notes |
|---:|---:|---|---|
| 0 | 4 | batch magic | `AJBH` |
| 4 | 1 | codec | `0` none, `1` zstd |
| 5 | 1 | checksum algorithm | `1` CRC-32C |
| 6 | 2 | flags | must be `0` |
| 8 | 8 | first sequence number | |
| 16 | 4 | record count | at least 1 |
| 20 | 4 | stored payload length | bytes on disk |
| 24 | 4 | raw payload length | bytes after decoding |
| 28 | 4 | header CRC | CRC-32C of bytes `0 .. 28` |
| 32 | *stored* | payload | encoded records |
| 32+*stored* | 4 | batch checksum | over the 32-byte header and the payload |
| 36+*stored* | 4 | commit trailer | `AJBC` |

The batch header has its own CRC, which is always CRC-32C. The reader can then trust the lengths, or reject them, before it reads the payload. The batch checksum algorithm is recorded per batch so it can be swapped later. Framing costs 40 bytes per batch.

### Payload and records

The decoded payload is one Protobuf message (`internal/journal/journalpb/journal.proto`):

```proto
message Batch  { repeated Record records = 1; }
message Record {
  uint64 seq = 1;            // contiguous across the journal, starting at 1
  string kind = 2;           // e.g. "tool.intent"; meaning defined by higher layers
  int64  time_unix_nano = 3; // when the record was appended
  bytes  data = 4;           // opaque to the storage layer
}
```

The storage layer doesn't interpret `kind` or `data`. Schemas for execution state, checkpoints and tool operations belong to higher layers and will get their own versioning.

Payloads are compressed with Zstandard at its fastest level, one frame per batch. If compression doesn't make a batch smaller, which is typical for single tiny records or random data, the batch is stored uncompressed with codec `0`.

Sequence numbers are contiguous across batches. A reader rejects a batch whose first sequence number doesn't follow the previous batch, or whose records aren't numbered consecutively from it.

## Writing and durability

A record goes through four levels. The journal only ever reports the third:

1. **Buffered:** handed to the writer, not yet in the file.
2. **Written:** in the file, which may mean only the OS page cache.
3. **Durable (acknowledged):** written and synced. `Append` returned a sequence number.
4. **Survives replacement:** on storage that outlives the container or Pod. This is the deployment's job: use a volume that persists.

To commit a batch, the writer:

1. encodes the records and compresses the payload;
2. writes header, payload, checksum and trailer with a single positioned write at the end of the valid data;
3. syncs the file;
4. only then answers every `Append` in the batch with its sequence number.

Records that arrive while a batch is being synced wait and go into the next batch together (group commit), so a sync is shared by every concurrent appender. `BatchPolicy` caps records and bytes per batch. It can also make the first record wait up to `MaxDelay` for others to join it.

**Creating** a journal uses exclusive create (it fails if the file exists), writes and syncs the header, and then syncs the parent directory. Without the directory sync, a crash could lose the directory entry and with it the whole file.

**Platform behaviour of "sync":**

- **Linux:** `fsync(2)`. On local filesystems such as ext4 and xfs this flushes the file's data and metadata and asks the device to flush its write cache.
- **macOS:** Go's `File.Sync` issues `fcntl(F_FULLFSYNC)`, which also drains the drive's cache. Plain `fsync(2)` on macOS doesn't guarantee that data reaches stable storage across a power loss. `F_FULLFSYNC` does, but it costs several milliseconds per call.
- **Windows:** `FlushFileBuffers`. Directories can't be synced there, so the directory sync on create is skipped.

The journal uses only portable file operations from the Go standard library: positioned writes, sync, truncate and, where available, `flock`. It relies on no particular kernel, filesystem or host feature.

### When a write or sync fails

Any failed or short write, or failed sync, **stops the writer**. The failing `Append` returns an error wrapping `ErrNotDurable`. Every later `Append` fails immediately with `ErrWriterFailed`, and the file isn't touched again. The journal has to be reopened, which recovers it as described below.

A failed sync is never retried. On Linux, after `fsync` reports an error the kernel may already have marked the unwritten pages clean or dropped them. A second `fsync` can then "succeed" without the data ever reaching disk.

### Unacknowledged records may survive

A batch can be fully written but not acknowledged: the process dies between the write and the end of the sync, or the sync fails after the data reached the disk anyway. Such a batch is complete and valid, so recovery keeps it. **Reopening can therefore show records that no caller ever saw acknowledged.** Higher layers must treat records after the last acknowledged sequence number as uncertain, for example by using stable IDs and reconciling them, and must never assume they don't exist.

`Append` also returns `ErrNotDurable` when its context ends after the record was handed to the writer: the record may still be written.

## Recovery

`Open` takes the lock, then reads the journal from the start and validates the framing, checksums and sequence numbers of every batch. It doesn't decompress or decode payloads, which makes opening a large journal roughly twenty times faster. It stops at the first problem and classifies it.

The rule that decides between truncating and refusing comes from how the writer works. It syncs every batch before it writes the next one, so at most the **final** batch can be unsynced at any moment. Damage confined to the final batch is therefore what an interrupted write or a power loss leaves behind, and it is truncated. Damage in a batch that another batch follows is in data that was already synced, so it is corruption.

| What the reader finds | Classification | What `Open` does |
|---|---|---|
| End of file exactly after a batch | clean end | append after it |
| Fewer bytes left than a batch header | **torn tail** | truncate, sync, append |
| A valid batch header whose batch extends past end of file | **torn tail** | truncate, sync, append |
| An invalid batch header, and only zero bytes until end of file | **torn tail** (the filesystem extended the file but the data never arrived) | truncate, sync, append |
| An invalid batch header, and no valid batch header anywhere after it | **torn tail** (damaged final batch) | truncate, sync, append |
| A bad commit trailer or checksum in a batch with only zeros or nothing after it | **torn tail** (damaged final batch) | truncate, sync, append |
| An invalid batch header with a valid batch somewhere after it | **corruption** | refuse to open, leave the file as is |
| A bad commit trailer or checksum in a batch followed by more data | **corruption** | refuse |
| A sequence gap between batches | **corruption** | refuse |
| A payload that passes its checksum but doesn't decode, or holds the wrong records | **corruption** | `Open` doesn't decode payloads, so this is reported when the records are read |
| A length or count above the configured limits | **limit exceeded** | refuse, even in the final batch |
| An unknown format version, codec, checksum or flag | **unsupported** | refuse, even in the final batch |

The committed prefix is everything before the first problem. Batches over the limits or in an unknown encoding are never truncated, even at the end of the file, because they may come from a newer or differently configured writer. Corruption in earlier data is never skipped or silently truncated. It is reported with its offset, and someone has to decide what to do. `actlet-cli journal inspect` shows exactly where it is.

`Open` reports what it found in `Writer.Recovery()`. `Truncated()` gives the number of bytes it dropped, and `TailErr` gives the reason. Callers should log every truncation.

The trade-off: if the final batch *was* acknowledged and the storage later damages it, it is dropped rather than refused. As with unacknowledged records that survive, higher layers must reconcile everything near the end of the journal rather than assume it.

The file header is written and synced before `Create` returns, so a journal with a damaged or missing header is reported as corrupt, never as torn.

Readers don't take the lock. `actlet-cli journal inspect` can look at a journal while it's being written, and it then usually sees a clean end, possibly a torn tail.

## Limits

Readers enforce these, and writers never produce anything beyond them. Defaults:

| Limit | Default |
|---|---|
| decoded batch payload | 16 MiB |
| stored batch payload | 16 MiB |
| records per batch | 65,536 |
| record data | 4 MiB |
| record kind | 128 bytes |
| session ID | 255 bytes |

Lengths are checked before anything is allocated or read. The Zstandard decoder is capped at the decoded batch limit, for both output size and window size. A small "decompression bomb" frame is therefore rejected after allocating about the limit, not the size it claims or expands to. An oversized record is rejected by `Append` with `ErrLimit` and doesn't use up a sequence number.

## Exclusive writer lock

The journal must have exactly one writer. Where it can, the writer takes `flock(LOCK_EX | LOCK_NB)` on the journal file and holds it until `Close`.

- A second writer, in another process or in the same process through a second `Open`, is refused with `ErrLocked`. Each open of the file has its own lock.
- The lock is released automatically when the process exits or is killed. There are no stale lock files to clean up.
- The file descriptor is close-on-exec, so child processes don't inherit the lock.
- Where the platform or filesystem can't lock (no `flock`, as on Windows, or a filesystem that returns `ENOTSUP` or `ENOLCK`), the writer **logs a warning and continues without the lock**. `Writer.Locked()` reports whether the lock is held. Running only one writer per session folder is then up to whoever deploys Actlet.

What the lock doesn't guarantee:

- **It is advisory.** Code that writes to the file without going through this package isn't stopped.
- **Shared and network filesystems can defeat it.** On Linux NFS, `flock` is emulated with NFS byte-range locks. Those depend on the lock service and on leases, and a lease that lapses during a network partition can let a second client take the lock while the first still believes it holds it. SMB/CIFS and FUSE filesystems vary, and some accept the lock without enforcing it.
- **It can't see other machines.** A writer on another node that mounts the same block device isn't stopped by a lock this node holds.

## Storage and deployment

Actlet runs wherever a container can, and it doesn't check or restrict where its session folder lives. What it guarantees, and what it leaves to whoever deploys it:

**Actlet guarantees** that an acknowledged append was written and synced, and that reopening a journal recovers the valid committed prefix, dropping only a torn final batch. Both hold only as far as the storage honours sync. Storage that acknowledges a sync it hasn't done can lose acknowledged records on power loss, and Actlet can't detect that. Examples are write caches without power-loss protection, and some virtual disks and network filesystems.

**Keeping the session folder is the deployer's job.** A container's own filesystem survives a restart of the same container, but not its replacement. To recover a session in a new container (a rescheduled Pod, a new task attempt, a host move), put the session folder on storage that outlives the container: a named volume, a host directory or a persistent volume claim, mounted at the same path. The image uses `/var/lib/actlet`. `make test-docker` shows both sides. It kills a writer container and recovers every acknowledged record in a new container on the same volume, then shows that the session is gone once the volume is deleted.

**Run one writer per session folder.** Actlet takes the exclusive lock where it can and refuses a second writer it can see. Where locking isn't available, or a shared network filesystem defeats it, nothing stops two instances from writing the same journal. Don't point two running instances at one session folder.

## What the journal does not protect against

- Losing or deleting the volume. The journal can only be as durable as the storage under it.
- Storage that lies about syncing: drive caches without power-loss protection, or a hypervisor or network block layer that acknowledges flushes it hasn't done.
- Media corruption in general. Checksums *detect* corruption; they can't repair it.
- Tampering. CRC-32C isn't a cryptographic hash, and compression isn't encryption. Don't store credentials in a journal.
- Side effects that already happened outside the journal, such as a command that ran or a file that was written, and workspace files the journal doesn't contain.

### A note on power loss

The process-kill tests show that a killed process leaves a valid journal: a write either completes in the page cache or doesn't happen. A power loss is different. Writes that haven't been synced can reach the disk partially and out of order, for example a batch's trailer without all of its payload. Because the writer syncs each batch before writing the next, only the final batch can be affected, and recovery truncates it as described above. The tests simulate these shapes directly: zeroed or garbled payloads, headers and checksums in the final batch.

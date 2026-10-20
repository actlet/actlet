# Compatibility fixtures

Files in each format version Actlet has written, kept so that later versions can be tested against them. Before 1.0 nothing is promised: the formats may still change incompatibly. The fixtures make that visible, and once 1.0 promises compatibility, they are how it is tested.

`v0/` is the demo session (`examples/demo`), without its pauses, run against `actlet-devgw`:

| File | What it is | Tested by |
|---|---|---|
| `session.actlet` | the session folder's journal | `TestJournal`: it recovers to the completed session, and opens for appending without truncating |
| `recording.actrec` | the gateway's activity recording | `TestRecording`: it holds the events of `events.jsonl` and folds into a view that has everything `view.json` has; a view may gain fields, never lose or change them |
| `events.jsonl` | `actlet-cli replay export --jsonl` of the recording | `TestRecording` |
| `view.json` | `actlet-cli replay view --json` of the recording | `TestRecording`, and `TestReplayViewCompat` in `cmd/actlet-cli`, from both the recording and the export |
| `profile.yaml`, `profile.json` | the demo's profile in both formats | `TestProfileAndTask`: both load to the same wire profile |
| `task.json` | the demo's task | `TestProfileAndTask` |

When a test fails, the code no longer reads what an earlier Actlet wrote, or no longer reads it the same way. Before 1.0, a deliberate change can regenerate `v0` (`make build`, then `go run test/compat/generate.go v0`) and say so in its commit. After 1.0, it can't.

## Adding a version

When a format's version changes, such as the journal's `FormatVersion` or the gateway protocol's `v1`:

1. Keep the existing folders as they are: they are what the new code must still read, or refuse clearly.
2. `make build`, then `go run test/compat/generate.go v1` writes the new version's fixtures into `v1/`.
3. Add `"v1"` to `versions` in `compat_test.go`. If a test's expectations differ between versions, such as a field only `v1` has, branch on the version in that test, not by changing the old fixtures.
4. Add a fixture test to `cmd/actlet-cli` like `TestReplayViewCompat` for the new folder.

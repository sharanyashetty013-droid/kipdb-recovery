# RECOVERY.md — Person 2: atomicity, recovery, compaction

Owner: Person 2. Code: `recovery/`. Run: `go test ./recovery -v`.

> **Status:** built and tested on Linux. The Windows code compiles (`GOOS=windows go vet`)
> but **must be run on a Windows machine before we claim it works** (see Risks).

---

## 1. Invariants this package guarantees

1. **No partial batch.** A batch is one frame. A frame is applied to the index only after
   its header CRC, payload CRC, every entry CRC and every structural check pass.
2. **No acknowledged loss from recovery.** Recovery only truncates the *last* frame, and only
   when it looks like an interrupted write. Damage with valid data after it is refused.
3. **No guessing.** Corrupt structure → `ErrCorruption`. Disk/OS failure → `ErrIO`.
   Neither ever truncates or deletes anything.
4. **Never delete the only good copy.** Leftover `.compact` / `.old` files are removed
   only after the authoritative file has passed recovery.
5. **Idempotent.** Recovery killed at any step, then rerun, gives the same result.
6. **One process at a time.** `Open` takes an exclusive OS lock first (`ErrLocked` otherwise).

## 2. What `Open` does

```
1. Lock  <db>.lock           flock (Unix) / LockFileEx (Windows). Fails fast with ErrLocked.
2. Pick the real file        authority rules (§3)
3. Recover(<db>)             scan frames, rebuild index, cut a torn tail (§4)
4. Clean up                  delete <db>.compact and <db>.old — only after step 3 succeeded
```

Returns a `Handle{File, Result, Path}`. Person 1's writer appends at `Result.ValidEnd`
using `Result.NextSeq`.

## 3. Authority rules (which file is the real database)

| Files on disk after crash | Meaning | Action |
|---|---|---|
| `db` | normal | use `db` |
| `db` + `db.compact` | crashed while building/verifying compact | use `db`, delete compact after recovery |
| `db` + `db.old` | crashed after the swap, before cleanup | use `db` (new, verified file), delete old |
| `db.old` + `db.compact` | crashed mid-swap | **roll back**: rename old → db, delete compact |
| `db.old` only | (not reachable by our protocol) | roll back, same as above |
| `db.compact` only | crashed while creating a brand-new db | `create=true`: make fresh db. Else `ErrNotExist` |
| nothing | new db | `create=true`: make fresh db. Else `ErrNotExist` |
| `db` corrupt + any leftover | real damage | `ErrCorruption`, **touch nothing** |

**Why roll back instead of forward?** `.old` was the live database a moment ago, so it
already holds every acknowledged write. `.compact` would need re-verifying. Rolling back
loses only the compaction, never data, and gives fewer cases to prove.

**Why `.compact` is never authoritative on its own:** it only becomes `db` through a
rename. Until that rename, `db` (or `db.old`) holds everything acknowledged.

## 4. Recovery scan (the three broken-tail cases)

| What's broken | Action | Why |
|---|---|---|
| Header CRC/magic bad | treat as torn tail, cut there | never search ahead: a user's value can look like a valid header |
| Header OK, payload CRC bad, frame ends at EOF | cut it | the interrupted last write |
| Header OK, payload CRC bad, data follows | `ErrCorruption`, no cut | a crash only tears the last frame; cutting would delete saved data |
| Frame runs past EOF | cut it | interrupted write |
| Checksums pass, structure wrong | `ErrCorruption` | not a crash; refuse to "fix" |
| Damage before `snapshot_end` | `ErrCorruption` | compacted area is protected |
| Read error | `ErrIO` | a failing disk is not end-of-file |

`Verify(f)` runs the same checks read-only, for `kvdb check`: torn tail → reported in `TornTailAt` (still OK); corruption → error.

Sequence numbers must equal `first_seq, first_seq+1, …` with no gaps or duplicates.

## 5. Compaction protocol

Caller must hold the write lock (no appends during compaction).

| # | Step | Crash here → disk state | Recovery result |
|---|---|---|---|
| 1 | create `db.compact`, write snapshot frames | `db` + partial compact | `db` wins, compact deleted |
| 2 | write superblock (`first_seq = NextSeq`, `snapshot_end = end`) | `db` + partial compact | same |
| 3 | fsync + close compact | `db` + full compact | same |
| 4 | reopen compact, `Recover` it, compare every value | `db` + full compact | same |
| 5 | close `db` (Windows can't rename open files) | unchanged | same |
| 6 | rename `db` → `db.old` (durable) | `db.old` + compact | roll back to old |
| 7 | rename `db.compact` → `db` (durable) | `db` + `db.old` | new `db` wins, old deleted |
| 8 | recover new `db`, delete `db.old` | `db` (+ maybe old) | new `db` wins |

Snapshot frames continue the sequence (`first_seq = old NextSeq`), so numbers never go backwards.

Failures:
- Steps 1–4 (incl. disk full, fsync failure) → `ErrCompactAborted`. Database unchanged, handle still usable.
- Steps 5–8 → we reopen (which rolls back). If that works, `ErrCompactAborted`.
  If even that fails, the handle is dead and the caller must `Open` again.

Durability of renames: Unix = `rename` + fsync parent dir. Windows = `MoveFileExW` with
`MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH`.

## 6. Test evidence

| Test | Proves |
|---|---|
| `TestTornTailAtEveryByte` | cut file at every byte of the last frame (a batch): always all-or-nothing, recovery idempotent |
| `TestFakeFrameInsideValueIsNeverParsed` | a value containing a valid-looking frame is never "found" |
| `TestBadPayloadCRCWithDataAfterIsCorruption` | middle damage is refused, file untouched |
| `TestNeverTruncateBelowSnapshotEnd` | protected snapshot never cut |
| `TestRealReadErrorIsNotEOFAndNeverTruncates` | I/O errors never truncate |
| `TestVerifyNeverWrites` | `check` mode (`Verify`) reports torn tails and corruption but never writes a byte |
| `TestAuthorityRules` | all 11 leftover-file combinations from §3 |
| `TestCompactionCrashAtEveryStep` | crash at every compaction step × 2 crash models (unsynced data lost / kept): state always exact, db writable after |
| `TestRecoveryInterruptedAfterCompactionCrash` | crash in compaction, **then crash again during recovery**, at every step: 38 scenarios |
| `TestCompactionDiskFull` | 8 disk-full points incl. short writes → aborted, data intact |
| `TestCompactionFsyncFailure` | fsync failure → aborted, data intact |
| `TestCrashDuringFreshCreate` | crash while creating a new db never leaves a half-trusted file |
| `TestLockIsExclusive`, `TestOpenFailureReleasesLock` | second opener gets `ErrLocked`; lock not leaked |
| `TestOSFSEndToEnd` | real disk: lock, compaction, leftover-file resolution |
| `BenchmarkRecoverOneMillionKeys` | ~1.0 s on Linux, in-memory file |

The test filesystem refuses to rename or delete an open file, like Windows does. So
Windows ordering bugs show up even when we test on Linux.

**Mutation check (do the tests catch real bugs?)** We planted each bug, all were caught:
skip `.compact` fsync · roll forward to unverified `.compact` · delete `.old` before verifying `db` ·
rename `db` while still open · truncate when data follows · skip payload CRC · truncate below `snapshot_end`.

## 7. Crash-here answers (Person 2's questions)

**Dies halfway through a header or payload?** Last frame is torn. Header bad or frame past
EOF or payload CRC bad at EOF → cut at that frame's start. Earlier frames untouched. The
interrupted op was never ACKed, so losing it is allowed.

**Dies after the batch frame is written but before fsync returns?** No ACK was sent. On
restart the frame is either fully there (CRCs pass → applied whole) or torn (cut whole).
Never half: the batch is one frame with one payload CRC.

**Can recovery apply a transaction twice?** No. Each frame has a unique sequence number
that must be exactly `previous + 1`. A duplicate is `ErrCorruption`.

**Recovery killed halfway?** Recovery only ever (a) truncates a torn tail, (b) renames
`.old` back, (c) deletes leftovers *after* verifying. Each is idempotent and each crash
state maps back into §3's table. Tested: 38 double-crash scenarios.

**Disk full during compaction?** Write fails (even a short write) → `ErrCompactAborted`,
`.compact` removed, `db` never touched. Tested at 8 fill points.

**Filesystem assumptions of compaction?** (1) rename within one directory atomically
replaces the target; (2) fsync of dir (Unix) / `WRITE_THROUGH` (Windows) makes it durable;
(3) fsync on a file means its data is on stable storage; (4) `.compact`, `db`, `.old` are on
the same volume.

## 8. Risks and open questions

1. **Not yet run on Windows.** Lock and `MoveFileExW` code compiles but is untested on a real
   Windows disk. Run `go test ./recovery` on Windows first thing.
2. **Format agreement with Person 1 (blocking).** Entry CRC assumed = entry header bytes 0–6 +
   key + value. If DESIGN.md differs, every file reads as corrupt. Check: `EncodeFrame` bytes
   must equal what Person 1's writer produces.
3. **Writer must update the index only after fsync.** `Compact` copies whatever is in
   `Result.Index`. If the writer forgets to update it, compaction silently drops those keys.
   (Our own test helper had exactly this bug at first; the end-to-end test caught it.)
4. **Damaged header in the middle is cut, not refused** (per spec). One flipped bit in a
   middle header deletes everything after it. Confirm with Person 1 this is intended.
5. **Our test filesystem treats each write as all-or-nothing.** Real disks can tear a single
   write. The torn-tail-at-every-byte test covers that for the log, but not for every
   compaction write. Person 3's torture tool on a real disk should cover it.
6. **No `.new` file.** We only use `.compact` and `.old`. If DESIGN.md requires `.new`, update §3/§5.
7. **Clock/TTL:** not handled here; TTL lives in the record format (Person 1 / extension).

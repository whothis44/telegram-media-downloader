# Group priorities and shared content (ix.11.1)

The account can be lost at any time and its download volume is limited, so not everything will arrive. This is how the archive decides what goes first. It does not change what Telegram allows, and it makes no claim about speed: ranking changes *which* bytes arrive, not how many.

## What a person controls

**Archive → Priorities** holds a ranked list of groups: drag a row or use the arrows. Tick several groups to put them first, last or at a rank together. A change shows at once and is saved in about a millisecond; rows already queued follow in the background within a few seconds, so the very next download may still come from the old order. The list is stored apart from the rescan queue (`tgdl_group_rank`), so it survives restarts, finished rescans and "Remove finished". The Rescan tab's **★ First** puts a group on the same list; groups starred by an earlier version join it once, on the first start.

Two policies:

- **Finish in order** (default): rank 1 until nothing of it can download, then rank 2. Lower groups may get little.
- **Spread coverage**: a group's weight is 1/rank, and the group with the least coverage per weight goes first, rechecked every 10 minutes. Coverage here means bytes saved or shared, so small pictures cannot make a group look finished. No group is promised to finish first.

## Where the ranking takes effect

| Step | What happens | Code |
|---|---|---|
| Discovery | Ranked groups go first in both rescan lanes. A higher group waiting for a lane stops a lower pass after it has run 2 minutes; the lower pass carries on from where it stopped, at no attempt's cost. | `ixQueueStep`, `ixQueueTrack` |
| Queue admission | Rows of files a group outranks do not count against its backfill's queue limit. Neither do rows that take no transfer of their own, for any group. | `ixAdmissionCredit`, `ixSharedWaiting`, `history_runner.go` |
| Download selection | Each queue row carries `sched_order`. The claim stays an index walk: nothing sorts the queue on a worker's poll. | `engine.claimPriority`, `ixSchedStep` |
| Slots | Waits for a file slot and for a part go by the same order. A file still waiting for its slot gives way to better work: no bytes have moved, no attempt is spent. | `telegram.WithDownloadRank`, `WorkStore.Outranked` |
| Deadline order | Files a ranked group needs are never set aside. Size and private/public bands become the tie-break within a rank, while time is short. | `ixDeadlineStep`, `ixSchedStep` |
| Duplicate checks | Sampling and preview fetches go through ranked groups first, and get a bounded turn under a long speed limit (below). | `ixSampleJobs`, `ix_remote_budget.go` |
| Holds | Probable copies stay held whatever the rank. Their rank goes to the better copy they wait for, and they are released in rank order when their stage comes. | `ixSchedHoldLinks`, `ixReleaseSome` |
| Reporting | Per group, in files and bytes separately (below). | `ix_coverage.go`, diagnostics `[group priorities]` |

Claim order, first to last: self-destructing media; explicit requests (a pasted link); ranked history and live downloads by rank; then unranked live; then unranked history. With nothing ranked the order is what it always was, except that explicit requests no longer share a lane with live downloads: they go first.

## A file in several groups

A file is one piece of work, whichever groups posted it:

- It goes at the **best rank of any group that needs it**, whichever group's row is in the queue, and even when the ranked group has only had its messages read and has not queued it yet (`ix_messages.media_key`).
- **One transfer at a time.** While a row downloads, rows of the same Telegram file stay in the queue instead of holding a worker on the library's lock.
- **Rows of a file already saved take no bytes.** They go first, need no fresh file reference (so no Telegram request), and are credited immediately. If the saved file proves to be missing, the second attempt refreshes and downloads it as usual.

"The same" here means the same Telegram identity. Exact re-uploads and look-alikes are not merged by this: they stay with the existing copy checks, whose evidence is reported separately.

## What the figures mean

A group's distinct files are counted once each, in the first of these that applies:

- **Saved**: in the library, and this group's message brought it.
- **Through others**: in the library through another group, as the same Telegram file or the same bytes confirmed by every part's hash.
- **Probable**: evidence only. A held copy, a look-alike of a saved file, or a re-upload compared on sampled regions (a big video's start, middle and end, or three samples). Two files can agree there and differ elsewhere, so these never count as saved.
- **Still needed**: nothing yet. "Also needed ahead" marks what a group going first needs too, which then comes free.

Global totals count each file once, however many groups have it; per-group credit is reported apart and never adds to them. A group not fully read says so, and its figures and estimates are incomplete. Files deleted on Telegram cannot be fetched, so they are not "needed". Finish estimates are ranges from the last 3 hours' and last day's speeds, and are estimates only: on this kind of account the speed changes by more than an order of magnitude within a day.

## Checking and downloading both cost something

On a speed-limited account, "Telegram is busy" stays true for days, so simply yielding meant the checks most worth their bytes never ran. Under pressure each kind of check now gets a bounded turn: at most 1 minute of big-video sampling and 30 seconds of preview fetching in each 10-minute window, the highest-ranked candidates first. Telegram's own `FLOOD_WAIT` is still a hard stop for ten minutes. A timeout remains "unknown": never a match and never a non-match.

A check's reads may now wait through Telegram's speed-limit waits (5 minutes a part, 10 in all, up from 45 seconds and 4), because a wait is not a failure. A sampled match only moves a file to the back of the queue: nothing is deleted or skipped for good. Very high-motion footage may not match its own fingerprint, so it reads as "no match" and downloads, which is the safe way to be wrong.

These numbers are engineering choices, not measured optima. Compare the `big videos sampled` and `previews` lines of the diagnostics over a few days before changing them.

## Limits

- **Running transfers keep the place they were claimed with.** They are not stopped when ranks change, because there is no resume across attempts and stopping would discard their bytes. A very large low-ranked file already downloading holds its file slot until it finishes, though its parts wait behind higher-ranked ones.
- **No durable resume.** A restart still loses a partial file.
- **One group per lane.** Only one group queues files at a time, so under Spread a rotation happens every 10 minutes at most, when the places change.
- **The overlap between groups is not predicted.** Demand comes from messages actually read, so unread groups contribute none. There is no prior estimate from a family of similar groups, and no rule that weighs a check against a download by expected value beyond the bounded turns and candidate order above.
- **Value is rank only.** All of a group's files are equal within a rank apart from the size tie-break; a lower-quality copy is not credited as provisional coverage.
- **Which release is running.** An updater that compares version strings will replace a local build with any public release that differs. Keep updates off for a local build until a release with this work is published.

## After a downgrade

The new column and indexes are additive. An older build can open the same database, but its claim no longer matches an index exactly, so it may be slower on a large queue.

# Regular-account download repair

Local build: `3.4.18-ix.10.2-regularfix`, based on `f5168c9`.

This build is tuned for a Telegram account that will remain non-Premium. It includes the earlier video-sampling crash fix.

## Behavior

- Repeated `FLOOD_PREMIUM_WAIT` responses wait and resume the same file part, including waits longer than the old inline-retry threshold. They no longer turn into a generic file refusal after four responses. Already-written bytes remain part of the active transfer. This is not disk-based resume across shutdowns.
- Time spent waiting for Telegram or an available file/part slot does not consume the queue's transfer timeout budget. Actual network requests still have timeouts. User cancellation and shutdown still interrupt waits.
- With adaptive pacing enabled, the initial window is two parts per datacenter, increasing by one at a time up to eight. These are conservative local tuning choices, not claims about a universal Telegram quota. Previously saved larger windows are clamped to eight.
- Trials use a 30-second baseline and a 60-second measurement period. Telegram waits suspend learning, including the slow-response reduction. Learning resumes with a fresh baseline. Only a stable window is persisted; an unproven trial does not become the restart default.
- Separate small/large file slots per datacenter use `small_queue_max_active_operations_count` and `large_queue_max_active_operations_count` from Telegram's app configuration, with a 20 MiB boundary. The fallback is four small and two large files. Configuration lookup has a five-second timeout and failure does not stop startup; existing freeze-state configuration reads also refresh these limits.
- Optional remote video samples and previews defer when parts are queued for capacity or Telegram recently throttled downloads. Already-kept previews can still be processed locally. Held-copy release also waits when its prerequisite sample checks are deferred.
- Privacy-safe diagnostics show current Telegram waiters, cumulative wait time summed across requests, and active small/large file slots and their limits. A currently active Telegram wait is distinguished from an unexplained stall.
- Windows drive-letter database paths are encoded correctly as file URIs. No schema change is introduced.

The account-wide file cooldown remains deliberately conservative. This change does not bypass Telegram's waits or guarantee a particular transfer rate. Optional work already underway is not forcibly interrupted just because another download begins.

## Verification

Synthetic tests cover repeated 65-second speed-limit waits after earlier bytes were written, exact output contents, immediate cancellation during an hour-long wait, balanced timeout-budget callbacks, long-wait learning suspension, simultaneous slow responses and speed-limit waits, stable-window persistence, configuration validation, independent datacenter/size limits, and deferred background work resuming without a recorded failure.

The tests use invented media and temporary databases. No live Telegram account or private downloaded media is needed. Existing core and archive regression suites are run separately; see the packaged validation notes for final results.

## Apply

Stop the downloader and close its restart window. Keep a backup of `core-service/tgdl-server.exe`, then replace that executable with this build. Use the replacement `start.bat` with automatic updating disabled, so the public 10.2 release does not overwrite the local fix. Keep the existing data, sessions, downloads, and configuration in place. Keep download pacing enabled to use the new adaptive settings.

After an uninterrupted run, compare fresh privacy-safe diagnostics: completed files/bytes, server-requested waits, live file slots, network response time, and cumulative wait time. Do not infer a speed improvement solely from more active workers or a larger part window.

Protocol references: [file downloads](https://core.telegram.org/api/files), [client configuration](https://core.telegram.org/api/config).

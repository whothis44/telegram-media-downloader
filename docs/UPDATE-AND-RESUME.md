# Updating and resuming without losing downloads

## Resume

Files of 8 MiB or more keep their progress across restarts, crashes and failed attempts, in the library and in the local staging folder alike (from ix.12.3; it was 64 MiB, library only). Smaller files still start over: they cost little to fetch again.

Telegram's parts reach the file in order, so progress is a single position. Every 8 MiB, or every 30 seconds if that comes first, a transfer syncs its bytes to disk, then records the position and the SHA-256 of everything before it in the database (`tgdl_part_progress`). The order matters: a record never claims more than was synced. Under Telegram's speed limit a file gets 8 MiB only every few minutes, so the clock is what sets how much a restart can lose then.

The interval was measured on the NAS with a scratch file, writing 1 MiB pieces as downloads do: a sync took 6 to 8 ms. Syncing every 8 MiB cost 2 to 5% only when writing flat out at about 23 MB/s, well above what downloads reach (every 2 MiB cost about 13%). That is one share and one computer, not a general figure.

An attempt coming back:

1. Finds the record for that message and Telegram file. A message edited to another file has another identity, so it does not match.
2. Reads the kept bytes and compares their hash with the record. **On any difference it starts the file from the beginning.** A wrong prefix would become a wrong file that the library then believes.
3. Cuts anything past the recorded position and asks Telegram from there.

The final hash covers the whole file, as it did before. The check reads the kept bytes once, so a resumed file over a slow network link takes a moment longer to start.

At startup, recovery removes partial files with no record and any kept part whose queue item is gone, finished or cancelled. A part in a staging folder that cannot be opened (a disk not there today) keeps its record. A part kept in one place is never carried on in another: when staging is turned on or off, or its folder changes, that file starts over and the old part is removed. The diagnostics show how many are kept and how many bytes.

### Stopping at a checkpoint

A transfer can be asked to stop: by an update waiting, or by a person pausing a file that is moving bytes. At the next part it records a checkpoint and ends there, losing nothing, and goes back to the queue with no attempt spent. A file with 8 MiB or less left finishes instead, and so does one too small to keep its progress. If the record cannot be written, it does not stop: it goes on and tries again at the next part.

### Limits

- A crash or a power cut can lose up to 8 MiB or 30 seconds of a file's progress, and more on a filesystem that reports a sync it did not do. The hash check catches that case, and the file starts over. An update or a pause loses nothing.
- `TGDL_VERIFY_CHUNKS=1` turns resume off: its reader always starts at the beginning.
- The crash tests kill a real process at chosen points, but the operating system survives those, so they test the order of the steps rather than what a power cut does to a disk's cache. That is why the bytes are checked rather than trusted.
- Nothing here has been measured against live Telegram.

## The update button

When a newer release exists, the Archive page offers **Update when downloads finish**:

- No new file starts, apart from self-destructing media, which may vanish while waiting.
- Files only waiting for a slot go back to the queue with no attempt spent. Files of 8 MiB or more that are moving bytes stop right after their next checkpoint and carry on after the restart; smaller and nearly finished ones finish. So the wait takes seconds to minutes.
- When nothing is left, the server exits and the launcher updates it and starts it again.

**Install updates automatically** (Settings → Downloads, off unless turned on) starts this by itself when a newer release is out, but only with a launcher that loops (below): otherwise the app would stop downloading and wait for a person. Going back to downloading leaves that version alone. If the downloads in progress have not finished within 20 minutes, it goes back to downloading and tries again an hour later.

**Update now** restarts at once, costing what the confirmation says. **Go back to downloading** cancels. While waiting, nothing moving does not count as a stuck queue, so set-aside and held files stay where they are. The state is in memory on purpose: a restart for any other reason never comes back refusing to download.

### The launcher

Exiting only brings the app back if something starts it again, so the launcher has to say that it does:

```bat
set TGDL_LAUNCHER_RESTARTS=1
```

`start.example.bat` has this. Without it the app never exits by itself: it says the downloads are done, and you close the window and open `start.bat`. Nothing is lost either way.

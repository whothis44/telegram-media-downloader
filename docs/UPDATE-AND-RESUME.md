# Updating and resuming without losing downloads

## Resume

Files of 64 MiB or more keep their progress across restarts, crashes and failed attempts. Smaller files still start over: they cost little to fetch again.

Telegram's parts reach the file in order, so progress is a single position. Every 64 MiB a transfer syncs its bytes to disk, then records the position and the SHA-256 of everything before it in the database (`tgdl_part_progress`). The order matters: a record never claims more than was synced.

An attempt coming back:

1. Finds the record for that message and Telegram file. A message edited to another file has another identity, so it does not match.
2. Reads the kept bytes and compares their hash with the record. **On any difference it starts the file from the beginning.** A wrong prefix would become a wrong file that the library then believes.
3. Cuts anything past the recorded position and asks Telegram from there.

The final hash covers the whole file, as it did before. The check reads the kept bytes once, so a resumed file over a slow network link takes a moment longer to start.

At startup, recovery removes partial files with no record and any kept part whose queue item is gone, finished or cancelled. The diagnostics show how many are kept and how many bytes.

### Limits

- Up to 64 MiB of a file's progress can be lost per restart, and more on a filesystem that reports a sync it did not do. The hash check catches that case, and the file starts over.
- A download to the local staging folder starts over. Only the library's own transfers keep progress.
- `TGDL_VERIFY_CHUNKS=1` turns resume off: its reader always starts at the beginning.
- The crash tests kill a real process at chosen points, but the operating system survives those, so they test the order of the steps rather than what a power cut does to a disk's cache. That is why the bytes are checked rather than trusted.
- Nothing here has been measured against live Telegram.

## The update button

When a newer release exists, the Archive page offers **Update when downloads finish**:

- No new file starts, apart from self-destructing media, which may vanish while waiting.
- Files only waiting for a slot go back to the queue with no attempt spent. Those moving bytes finish.
- When nothing is left, the server exits and the launcher updates it and starts it again.

**Update now** restarts at once, costing what the confirmation says. **Go back to downloading** cancels. While waiting, nothing moving does not count as a stuck queue, so set-aside and held files stay where they are. The state is in memory on purpose: a restart for any other reason never comes back refusing to download.

### The launcher

Exiting only brings the app back if something starts it again, so the launcher has to say that it does:

```bat
set TGDL_LAUNCHER_RESTARTS=1
```

`start.example.bat` has this. Without it the app never exits by itself: it says the downloads are done, and you close the window and open `start.bat`. Nothing is lost either way.

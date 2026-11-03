# Moving to ix.6 (v3.4.18-ix.6)

## Before you start (2 minutes)

1. **Copy the data folder** `T:\tg-dl\data` to the NAS or a USB drive. It holds the Telegram login
   session (a frozen account cannot sign in again) and the archive database.
2. Leave the downloads folder alone: nothing in it is moved or renamed by the update.

## Updating

1. Close the window running `start.bat` (or press Ctrl+C in it). Downloads in progress pick up again
   after the restart: the queue is kept in the database.
2. Double-click `start.bat`. It fetches v3.4.18-ix.6 by itself, keeps the old program as
   `tgdl-server.exe.previous`, and starts the app.
3. The window shows `Installed tgdl-server 3.4.18-ix.6.` Then open the dashboard and **Archive**: the
   Overview now has an "Open the media browser" button.

To go back: close the window, rename `tgdl-server.exe.previous` to `tgdl-server.exe`, and start the app
with `run_safe.bat` (start.bat would update it again). The database stays usable by the old version:
ix.6 only adds tables and columns.

## What happens to what you already have

- **Groups, categories, finished marks, ★ priorities and the rescan queue**: unchanged. The queue continues
  where it stopped.
- **The download queue**: unchanged. From now on, before a file is downloaded:
  - an exact re-upload (a picture, video or audio file of 2 MB or more, exactly as large as one already
    saved) has its bytes checked on Telegram; if they match it is catalogued like a forward and not
    downloaded. Documents are always downloaded.
  - a probable copy (a video of 5 MB or more, as long, as large and with the same preview as one already
    saved) waits at the back of the queue until nothing else is waiting (or the queue stalls for half an
    hour), then downloads.
  These checks use details the database already holds; nothing has to be scanned first.
- **Duplicates already downloaded**: stay where they are. The media browser folds them together for
  viewing. The only clean-up offered is a reviewable, undoable plan that moves byte-identical extra
  copies to `_trash`; it never runs by itself.
- **Video near-duplicate results**: rebuilt once with the new method (fixed time grid, black bars cut,
  mirror images). This runs in the background on the graphics card.

## What runs in the background after the first start

| When | What | Uses Telegram? |
|---|---|---|
| 2 min, then every 6 h | Reads the freeze state; if a deletion date is set, a red countdown appears and the rescan queue takes private chats first | yes (one tiny request) |
| End of every messages pass | Asks Telegram about every missing message number: kept, or recorded as gone | yes (gentle, 1 request/s) |
| Continuously | Picture index: reads each unique picture once (checksum, thumbnail, fingerprint) | no |
| Continuously | Video fingerprints on the graphics card | no |
| About 1/30 of the library a day | Integrity check: re-reads files and compares checksums | no |
| 3 min after start, then on changes | Builds the media browser's index | no |

Duplicate finding and integrity checks follow the "pause between … and …" hours under Archive →
Duplicates, and can be switched off in Settings.

## Recommended first steps

1. **Archive → Overview**: check the red deletion banner (if Telegram reports a date) and the
   "Copies, duplicates and integrity" card.
2. **Rescan → Rescan all** (finished groups are skipped). Each messages pass now also checks every missing
   message number and saves invite links; the Overview then shows "IDs 1–N: … accounted for" per group.
3. **Open the media browser** (button on the Overview, or `/ix/browse`): Originals only, Every post, Media
   grid, Read a chat, Review look-alikes, Collections & files, Links & wallets.
4. Keep the app running. Never log the account out.

## If something goes wrong

Archive → Settings → **Diagnostics report** → *Make the report* → read it → *Copy it* → paste it to
whoever is helping. Chats, files, people and links are replaced by placeholders; no message text is
included.

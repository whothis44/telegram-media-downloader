# After the deletion date

When Telegram freezes an account, it gives a date on which it will delete it (Archive → Overview shows it). A frozen
account cannot sign in again, so the app's saved Telegram session is the only way in until then. This page says what
the app does before and after that date, and how to put things back from the backups.

## Before the date

- **Nightly backup.** Every night, a minute after the index copy (Archive → Settings → Index copy on the NAS), the
  app copies the Telegram session files and the database into a folder next to the index copy:
  `<index folder> backups`, for example `X:\Telegram index backups`. Each copy is a folder named by its date and
  time. The last 7 nightly copies are kept; older ones are removed. It is on unless you untick it in Settings.
- **Every copy is checked.** Sizes and SHA-256 checksums are compared with the originals, and SQLite's integrity
  check is run on the database copy. Archive → Overview shows the last backup (time, size, OK or failed); the
  diagnostics report has the same numbers. A failed night is tried again an hour later, three times at most.
- **The session stays encrypted.** The session files are copied as they are. The key that opens them,
  `secret.key` in the app's data folder, is **never** copied into the backups folder. To restore a session you
  need that key too, so keep a copy of the whole data folder somewhere safe as well.
- **The day before.** When the deletion date is less than a day away, the app writes one more index copy and
  backup at once. That backup is named `<date and time> final` and is never removed by the rotation. The Overview
  then reminds you to keep the app running (it keeps saving for as long as Telegram allows) and to copy the data
  folder somewhere safe.

## After the date: read-only archive

When Telegram reports that the account is deleted or deactivated, or that the session is no longer valid
(`USER_DEACTIVATED`, `AUTH_KEY_UNREGISTERED`, `SESSION_REVOKED` and similar), the Overview says **"Telegram account gone: the archive is read-only now"** and the app stops all Telegram work:

- the download engine stops and is not restarted;
- rescans do not start, and freeze checks stop;
- anything that would open a Telegram connection (chat lists, stories, links) is refused with a short message.

The deletion date alone does not switch it on: Telegram may delete the account later than it said, and the app keeps
downloading for as long as Telegram still answers.

It does not keep retrying the dead session, so nothing loops and the log says it once.

Everything that works from what was saved keeps working, without Telegram:

- the media browser, search, and the index (in the dashboard and the copy on the NAS);
- image recognition, text in pictures, faces and people, speech to text, duplicate finding;
- the nightly index copy and the nightly backup.

If an appeal is accepted and the account works again, press **Try Telegram again** on the Overview. The app then
starts monitoring again (if it was on). If Telegram still refuses, the archive goes back to read-only.

## Restoring from a backup

Each backup folder holds:

| File | What it is |
|---|---|
| `db.sqlite` | The app's database: messages, file list, settings, recognition results |
| `sessions/` | The Telegram session files, exactly as the app keeps them |
| `session.enc` | Only on older installs: the first account's session |
| `manifest.json` | Size and SHA-256 of every file in the folder |
| `README.txt` | These steps, in short |

The downloaded files themselves are not in the backup; they stay in your downloads folder.

To restore:

1. Stop the app.
2. Pick the backup to use: the newest one, or the `final` one made the day before the deletion date. If you want,
   check the files against `manifest.json` (for example with `certutil -hashfile db.sqlite SHA256` on Windows or
   `sha256sum db.sqlite` elsewhere).
3. Put `db.sqlite`, the `sessions` folder and `session.enc` (if there is one) into the app's data folder,
   replacing the ones there. Keep the old ones aside until the app runs again.
4. Make sure `secret.key` from your saved copy of the data folder is in the data folder too. Without the same key
   the session files cannot be opened (the archive itself still opens: only the Telegram session needs the key).
5. Start the app.

After the deletion date the session is of no further use. To get the archive back on a new installation,
`db.sqlite` is what matters, together with the downloads folder it lists.

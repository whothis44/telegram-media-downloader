# Sample archive data (invented)

This folder is a **complete, made-up example** of the data files the archive writes next to its index
(`<downloads folder> index/data/`). Every chat, person, message and file here is invented; none of it
comes from a real archive. It is produced by the app's real exporter (the test `TestIxDataSample`),
and the tests fail if the exporter's output stops matching it, so it always shows the current format.

Use it to design and test tools without touching real data: give a developer (or an AI) this folder,
`data/README.md` (the format) and `schema.json` (a JSON Schema for every record), and have them build
against it. The tool then runs on the real `data` folder unchanged.

## What is in it

| Path | What |
|---|---|
| `data/README.md` | The format description, as written into every real export |
| `data/manifest.json` | Format version, export time, app version, downloads folder |
| `data/groups.jsonl` | The four chats, with their Telegram details (description, members, message count, discussion group) |
| `data/messages/g_<chat>.jsonl.gz` | Messages, one file per chat, gzip-compressed as in a real export |
| `data/messages/g_<chat>.jsonl` | The same, uncompressed, for reading (a real export has only the `.gz`) |
| `data/files.jsonl` | Every downloaded file |
| `data/peers.jsonl`, `data/topics.jsonl` | Names of senders and channels; forum topic titles |
| `data/near_duplicates.jsonl` | Videos that look the same |
| `schema.json` | JSON Schema (draft 2020-12) for each kind of record |

The media files themselves are not included; `path` values point to where they would be under the
downloads folder.

## The chats, and what each shows

**Riverside Cycling Club** (channel, `g_1811230001`)
- 101: signed post with bold text and a link: see `raw.entities`, `post_author`, `views`.
- 102–104: an album of three photos (same `album_id`), caption on the first.
- 105: a video, downloaded; `comments: 2`. The comments are in the club's discussion group (below).
- 106: a GPX document that is wanted but not downloaded yet: `file_status: "missing"`.
- 107: an edited post: `edit_date`, and the earlier text in `edits`.
- 108: deleted on Telegram since it was archived: `deleted_at`.
- 109: forwarded from another channel (Velo Weekly): `forwarded_from`, `forwarded_date`.
- 110: a sticker, switched off for this chat: `file_status: "not_wanted"`.
- 111: a system notice: the admin pinned 101 (`service: "messageActionPinMessage"`, `reply_to: 101`).
- 120–129: one post of 7 photos and 3 videos (one `album_id`), caption on 120; the last clip (129) is still
  missing, so the album is partly downloaded.

**Trail Builders Forum** (forum supergroup, `g_1811230002`; topics in `topics.jsonl`)
- 14, 27: system notices opening the topics "Trail reports" and "Tools & repairs" (`service: "messageActionTopicCreate"`).
- 5, 303: in General (no `topic_id`).
- 201, 202: in "Trail reports" (14); 202 replies to 201 (`reply_to`).
- 203: the club's recap video forwarded here. Same `telegram_file` and same `sha256` as club 105, and the
  same `path`: one file on disk, two messages.
- 204: a 21-second clip cut from the recap (found as a near-duplicate, kind `clip`).
- 301: a voice note (`media.type: "voice"`), 302: a PDF still missing.

**Riverside Cycling Club chat** (`g_1811230004`): the club's discussion group
- 9001, 9002: comments on club post 105 (`comment_on`), replying to the group's automatic copy of the post.

**Ottawa Maker Space** (`g_1811230003`, marked finished: `finished_at`)
- Files the downloader fetched before the archive existed; their messages were never captured. Each
  file still gets a line (`date` 0, no `raw`), so nothing on disk is left out. One of them is a
  720p re-encode of the club's recap: a near-duplicate of kind `same`.

## Typical jobs for a tool

- **Albums**: group a chat's messages by `album_id`; order by `message_id`; the caption is the
  non-empty `text` among them. Show the post as Telegram did: one card, all items in a mosaic.
- **Completeness**: compare a group's `messages` with `telegram_message_count` in `groups.jsonl`.
- **Comments**: join a channel post to the messages whose `comment_on` points at it.
- **Exact duplicates**: group `files.jsonl` by `sha256` (or by `telegram_file` when the hash is not
  there yet). Records sharing a `path` are already one file on disk.
- **Near duplicates**: `near_duplicates.jsonl` lists sets with every member's size, length and the
  messages it appears in; `pairs` say which are the same video and which are clips (and where the clip
  starts). Keeping the largest member of a `same` pair loses nothing.
- **Inventory**: count `file_status` per chat; `missing` lines are what is still to be fetched.
- **Full detail**: anything not lifted into its own field (reactions, buttons, link previews, exact
  media sizes) is in `raw`; `tl_base64` is the original bytes for MTProto libraries.

To make a fresh copy of this sample from the code: run the archive tests with
`IX_SAMPLE_OUT=<folder>` set (CI keeps one as the `archive-data-sample` artifact).

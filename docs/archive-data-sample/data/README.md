# Telegram archive: data files

Everything the archive knows, in plain files for other tools. Written with the index (nightly, or with "Write the index to the NAS now"). Format version 2; `manifest.json` says which version a folder holds.

All files are UTF-8 [JSON Lines](https://jsonlines.org/): one JSON object per line. Message files are gzip-compressed.

## Files

| File | One line per |
|---|---|
| `manifest.json` | (one object) export time, app version, `downloads_root`: the downloads folder as the server saw it |
| `groups.jsonl` | chat: `group_id`, `chat_id`, `name`, `category`, `download_settings`, counts, `messages_file`; from Telegram's chat details: `telegram_title`, `type` (channel, supergroup, forum, group), `forum`, `about`, `username`, `members`, `pinned_message_id`, `discussion_group_id`, `telegram_message_count` (what Telegram says the chat holds; compare with `messages`), `info_tl_base64`; `finished_at` when marked finished |
| `messages/g_<chat_id>.jsonl.gz` | message of that chat, oldest first |
| `files.jsonl` | downloaded file: `group_id`, `download_id`, `path`, `size`, `type`, `sha256`, `telegram_file`, `created_at`, and when known `mime`, `width`, `height`, `duration_s`, `thumb_stripped_base64` |
| `peers.jsonl` | sender or source: `peer_id` (`user:<id>`, `channel:<id>`, `chat:<id>`), `name`, `username` |
| `topics.jsonl` | forum topic: `group_id`, `topic_id`, `title` |
| `near_duplicates.jsonl` | set of videos that look the same (re-encodes, clips): `kind` (`same`, `mixed`); `members` (largest first: `download_id`, `path`, `sha256`, `telegram_file`, `size`, `duration_s`, `places` = the messages it appears in); `pairs` (`a`, `b` = member indexes, `kind` `same` or `clip`, `score`, `offset_s` = where the clip starts) |

## IDs

- `group_id` is the downloader's marked chat ID (channels and supergroups: `-100` followed by `chat_id`). `chat_id` is Telegram's own ID.
- A message is identified by (`group_id`, `message_id`).
- `album_id` is Telegram's `grouped_id`: messages sharing it in a chat are one album (one post with several photos/videos). The caption is usually on one of them.
- `topic_id`: forum topic (0 = none; in forums, messages without a topic belong to General, topic 1).
- `reply_to`: the message replied to, in the same chat.
- `sender` and `forwarded_from` are `peer_id`s (see `peers.jsonl`); `forwarded_from` can also be a plain name, or `hidden`.

## Messages

`date`, `edit_date`, `forwarded_date`: Unix seconds (UTC). `first_seen`, `last_seen`, `deleted_at`, `recorded_at`: Unix milliseconds, when the archive saw it.

- `media`: what the message carries on Telegram (`type`: photo, video, document, audio, voice, gif, sticker...; `name`; `size` in bytes; `telegram_file`; `mime`, `width`, `height`, `duration_s`; `thumb_stripped_base64`: Telegram's tiny "stripped" preview, a JPEG body without its standard header, see core.telegram.org/api/files#stripped-thumbnails), or null.
- `file_status`: `downloaded` (see `files`), `missing` (wanted but not downloaded), `not_wanted` (switched off for the chat), `no_media`.
- `files`: the downloaded file(s), same fields as `files.jsonl`.
- `edits`: earlier versions of the text, as they were seen.
- `deleted_at`: set when a full rescan no longer found the message on Telegram (it is kept here).
- `raw`: the complete Telegram message, decoded: every object has `_` (its TL constructor name, such as `message` or `messageMediaPhoto`) and its fields in the TL schema's snake_case; bytes are base64. Fields that are 0, false, empty or absent are left out: read a missing number as 0 and a missing flag as false (an entity at the start of the text has no `offset`). Formatting (`entities`), reactions, buttons and full media details are here.
- `tl_base64`: the same message as Telegram's original binary (TL) serialisation, for exact decoding with any MTProto library (gotd/td, Telethon...).
- A line with no `raw` and `date` 0 stands for a downloaded file whose message was never captured.
- `service`: set on system notices (pinned a message, changed the title or photo, created a topic, members joined...): the TL action name; `text` describes it, `raw._` is `messageService`.
- `comments`: on a channel post, how many comments Telegram reports. The comments themselves are messages of the channel's discussion group (`discussion_group_id` in `groups.jsonl`), in that group's own messages file, each with `comment_on` pointing at the channel post (`group_id`, `message_id`).

## Files on disk

`path` is relative to `downloads_root`, with `/` separators. `sha256` is the SHA-256 of the file's bytes when the app has computed it (it may be missing for some files; compute it yourself if needed).

## Duplicates

- Exactly the same file: equal `sha256`, or equal `telegram_file` (the same Telegram upload, for example forwarded to several chats; `kind:id:size`).
- Same video, different file (re-encoded, resized or cut): `near_duplicates.jsonl`.

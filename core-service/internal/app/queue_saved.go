package app

import (
	"context"
	"strings"
)

// queueAddSavedPaths gives a finished row with no file of its own the file the
// library saved for it: the same Telegram file saved for another message, or a
// saved file with the same bytes (an exact re-upload, ix_subst). Without it such a
// row could not be opened, and nothing said why.
func (a *App) queueAddSavedPaths(ctx context.Context, snapshot map[string]any) {
	items, _ := snapshot["recent"].([]map[string]any)
	for _, item := range items {
		if item["status"] != "done" {
			continue
		}
		if p, _ := item["filePath"].(string); p != "" {
			continue
		}
		group, _ := item["groupId"].(string)
		msg, ok := item["messageId"].(int64)
		if group == "" || !ok {
			continue
		}
		if path := a.queueSavedPath(ctx, group, msg); path != "" {
			item["filePath"] = path
		}
	}
}

func (a *App) queueSavedPath(ctx context.Context, group string, msg int64) string {
	var path string
	if a.db.Reader.QueryRowContext(ctx, `SELECT of_path FROM ix_subst WHERE group_id=? AND message_id=? AND of_path<>''`, group, msg).Scan(&path) == nil && path != "" {
		return path
	}
	var identity string
	if a.db.Reader.QueryRowContext(ctx, `SELECT identity FROM tgdl_work WHERE group_id=? AND message_id=? ORDER BY id DESC LIMIT 1`, group, msg).Scan(&identity) != nil {
		return ""
	}
	parts := strings.Split(identity, "\x00")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT file_path FROM downloads WHERE telegram_media_kind=? AND telegram_media_id=?
		AND COALESCE(file_path,'')<>'' AND (status IS NULL OR status='completed') ORDER BY id LIMIT 1`, parts[0], parts[1]).Scan(&path)
	return path
}

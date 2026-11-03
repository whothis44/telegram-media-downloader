package telegram

import (
	"context"
	"errors"
	"fmt"

	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// Archive additions: the account's freeze state, messages read by ID (to
// close gaps in a chat's history), and cheap checks of a file's bytes on
// Telegram without downloading it.

// FreezeState is what Telegram's app configuration says about a frozen
// account: when it was frozen, when it will be deleted unless an appeal is
// accepted, and where to appeal. All zero when the account is not frozen.
type FreezeState struct {
	Since     int64  `json:"since,omitempty"`
	Until     int64  `json:"until,omitempty"`
	AppealURL string `json:"appealUrl,omitempty"`
}

// FreezeState reads freeze_since_date, freeze_until_date and
// freeze_appeal_url from help.getAppConfig.
func (a *Account) FreezeState(ctx context.Context) (FreezeState, error) {
	result, err := a.API().HelpGetAppConfig(ctx, 0)
	if err != nil {
		return FreezeState{}, err
	}
	cfg, ok := result.(*tg.HelpAppConfig)
	if !ok {
		return FreezeState{}, errors.New("Telegram sent no app configuration")
	}
	return freezeFromConfig(cfg.Config), nil
}

func freezeFromConfig(v tg.JSONValueClass) FreezeState {
	var st FreezeState
	obj, ok := v.(*tg.JSONObject)
	if !ok {
		return st
	}
	for _, kv := range obj.Value {
		switch kv.Key {
		case "freeze_since_date":
			if n, ok := kv.Value.(*tg.JSONNumber); ok {
				st.Since = int64(n.Value)
			}
		case "freeze_until_date":
			if n, ok := kv.Value.(*tg.JSONNumber); ok {
				st.Until = int64(n.Value)
			}
		case "freeze_appeal_url":
			if s, ok := kv.Value.(*tg.JSONString); ok {
				st.AppealURL = s.Value
			}
		}
	}
	return st
}

// ByIDPage is the result of reading messages by ID: the messages Telegram
// still has, and the IDs it reports as empty (deleted, or never sent).
type ByIDPage struct {
	Messages []tg.MessageClass
	Entities *tg.Updates
	Gone     []int
}

// MessagesByID reads up to 100 messages of a channel or supergroup by ID
// (channels.getMessages). Basic groups number messages per account, so a gap
// there means nothing and they are refused.
func (a *Account) MessagesByID(ctx context.Context, dialog Dialog, ids []int) (ByIDPage, error) {
	var page ByIDPage
	peer, ok := dialog.peer.(*tg.InputPeerChannel)
	if !ok {
		return page, errors.New("messages are read by ID only in channels and supergroups")
	}
	if len(ids) == 0 {
		return page, nil
	}
	if len(ids) > 100 {
		return page, errors.New("at most 100 messages per request")
	}
	input := make([]tg.InputMessageClass, 0, len(ids))
	for _, id := range ids {
		input = append(input, &tg.InputMessageID{ID: id})
	}
	result, err := a.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: &tg.InputChannel{ChannelID: peer.ChannelID, AccessHash: peer.AccessHash}, ID: input})
	if err != nil {
		return page, err
	}
	raw, users, chats, err := historyMessages(result)
	if err != nil {
		return page, err
	}
	page.Entities = &tg.Updates{Users: users, Chats: chats}
	asked := map[int]bool{}
	for _, id := range ids {
		asked[id] = true
	}
	for _, m := range raw {
		if m == nil {
			continue
		}
		if e, ok := m.(*tg.MessageEmpty); ok {
			if asked[e.ID] {
				page.Gone = append(page.Gone, e.ID)
			}
			continue
		}
		if asked[m.GetID()] {
			page.Messages = append(page.Messages, m)
		}
	}
	return page, nil
}

// fileDC runs fn with an API client on the datacenter that stores the file.
func (c *GotdClient) fileDC(ctx context.Context, a Attachment, fn func(*tg.Client) error) error {
	if c == nil || c.client == nil || a.Location == nil || a.DC <= 0 {
		return errors.New("invalid Telegram file request")
	}
	g := guardedClient{c.client, c}
	var pool gotd.CloseInvoker
	var err error
	if a.DC == g.Config().ThisDC {
		pool, err = g.Pool(1)
	} else {
		pool, err = g.DC(ctx, a.DC, 1)
	}
	if err != nil {
		return err
	}
	defer pool.Close()
	return fn(tg.NewClient(pool))
}

// FileHashes asks Telegram for the SHA-256 hashes of the parts of the file
// starting at offset (upload.getFileHashes). Telegram keeps no hashes for
// some files and answers with an error.
func (c AttachmentClient) FileHashes(ctx context.Context, offset int64) ([]tg.FileHash, error) {
	g, ok := c.Source.(*GotdClient)
	if !ok {
		return nil, errors.New("Telegram file hashes are unavailable")
	}
	var out []tg.FileHash
	err := g.fileDC(ctx, c.Media, func(api *tg.Client) error {
		var e error
		out, e = api.UploadGetFileHashes(ctx, &tg.UploadGetFileHashesRequest{Location: c.Media.Location, Offset: offset})
		return e
	})
	return out, err
}

// ReadPart downloads the 1 MiB part of the file at offset (a multiple of
// 1 MiB); the last part is shorter.
func (c AttachmentClient) ReadPart(ctx context.Context, offset int64) ([]byte, error) {
	if offset%downloadPartSize != 0 {
		return nil, fmt.Errorf("part offset %d is not a multiple of %d", offset, downloadPartSize)
	}
	g, ok := c.Source.(*GotdClient)
	if !ok {
		return nil, errors.New("Telegram file parts are unavailable")
	}
	var out []byte
	err := g.fileDC(ctx, c.Media, func(api *tg.Client) error {
		var e error
		out, e = fetchPart(ctx, api, c.Media.Location, offset)
		return e
	})
	return out, err
}

// PartSize is the size of the parts ReadPart returns.
const PartSize = downloadPartSize

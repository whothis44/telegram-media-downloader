package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/sha3"
)

// Things worth following across chats, picked out of message text with
// fixed rules (no guessing): Telegram links and @usernames, e-mail
// addresses, phone numbers written in international form, cryptocurrency
// addresses (checked with their own checksums: Base58Check for Bitcoin,
// Litecoin, Dogecoin and TRON, Bech32 for native SegWit, EIP-55 for mixed-
// case Ethereum), Monero addresses (by shape) and IBANs (mod 97).

type ixEntityRef struct {
	Group string `json:"group"`
	Msg   int    `json:"msg"`
	Date  int64  `json:"date"`
}

type ixEntity struct {
	Kind  string        `json:"kind"`
	Value string        `json:"value"`
	Count int           `json:"count"`
	Chats int           `json:"chats"`
	First int64         `json:"first"`
	Last  int64         `json:"last"`
	Refs  []ixEntityRef `json:"refs,omitempty"`
	chats map[string]bool
}

const ixEntityRefs = 200

var (
	ixReTme    = regexp.MustCompile(`(?i)\b(?:https?://)?(?:www\.)?(?:t\.me|telegram\.me|telegram\.dog)/(\+[A-Za-z0-9_-]{6,}|joinchat/[A-Za-z0-9_-]{6,}|[A-Za-z][A-Za-z0-9_]{3,31})`)
	ixReMent   = regexp.MustCompile(`(?:^|[^\w@./])@([A-Za-z][A-Za-z0-9_]{4,31})\b`)
	ixReEmail  = regexp.MustCompile(`\b[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,253}\.[A-Za-z]{2,24}\b`)
	ixRePhone  = regexp.MustCompile(`\+\d[\d \-().]{6,18}\d`)
	ixReB58    = regexp.MustCompile(`\b[13LMDT][1-9A-HJ-NP-Za-km-z]{25,34}\b`)
	ixReBech   = regexp.MustCompile(`(?i)\b(?:bc1|ltc1)[ac-hj-np-z02-9]{11,71}\b`)
	ixReEth    = regexp.MustCompile(`\b0x[a-fA-F0-9]{40}\b`)
	ixReXmr    = regexp.MustCompile(`\b[48][0-9AB][1-9A-HJ-NP-Za-km-z]{93}\b`)
	ixReIBAN   = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{1,4}){3,9}\b`)
	ixB58Alpha = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
)

// ixBase58Check decodes a Base58Check string; ok when its checksum holds.
func ixBase58Check(s string) (version byte, ok bool) {
	n := new(big.Int)
	for _, r := range s {
		i := strings.IndexRune(ixB58Alpha, r)
		if i < 0 {
			return 0, false
		}
		n.Mul(n, big.NewInt(58))
		n.Add(n, big.NewInt(int64(i)))
	}
	b := n.Bytes()
	for _, r := range s {
		if r != '1' {
			break
		}
		b = append([]byte{0}, b...)
	}
	if len(b) < 5 {
		return 0, false
	}
	body, sum := b[:len(b)-4], b[len(b)-4:]
	h1 := sha256.Sum256(body)
	h2 := sha256.Sum256(h1[:])
	if string(h2[:4]) != string(sum) {
		return 0, false
	}
	return body[0], len(body) == 21
}

// ixBech32 checks a Bech32 or Bech32m string's checksum.
func ixBech32(s string) bool {
	s = strings.ToLower(s)
	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		return false
	}
	const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	hrp, data := s[:pos], s[pos+1:]
	values := make([]int, 0, len(hrp)*2+1+len(data))
	for _, c := range hrp {
		values = append(values, int(c)>>5)
	}
	values = append(values, 0)
	for _, c := range hrp {
		values = append(values, int(c)&31)
	}
	for _, c := range data {
		i := strings.IndexRune(charset, c)
		if i < 0 {
			return false
		}
		values = append(values, i)
	}
	gen := []int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := 1
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ v
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk == 1 || chk == 0x2bc830a3
}

// ixEIP55 accepts an all-lower or all-upper Ethereum address, or a mixed-
// case one whose capitals match its EIP-55 checksum.
func ixEIP55(addr string) bool {
	hexPart := addr[2:]
	if hexPart == strings.ToLower(hexPart) || hexPart == strings.ToUpper(hexPart) {
		return true
	}
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(strings.ToLower(hexPart)))
	sum := hex.EncodeToString(h.Sum(nil))
	for i, c := range hexPart {
		if c >= 'a' && c <= 'f' && sum[i] >= '8' || c >= 'A' && c <= 'F' && sum[i] < '8' {
			return false
		}
	}
	return true
}

// ixIBAN checks an IBAN's length and mod-97 check digits.
func ixIBAN(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	r := s[4:] + s[:4]
	mod := 0
	for _, c := range r {
		switch {
		case c >= '0' && c <= '9':
			mod = (mod*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			v := int(c-'A') + 10
			mod = (mod*100 + v) % 97
		default:
			return false
		}
	}
	return mod == 1
}

// ixExtract lists the entities in one text.
func ixExtract(text string) [][2]string {
	var out [][2]string
	add := func(kind, value string) { out = append(out, [2]string{kind, value}) }
	if strings.Contains(text, "t.me") || strings.Contains(text, "telegram.") {
		for _, m := range ixReTme.FindAllStringSubmatch(text, -1) {
			v := m[1]
			if !strings.HasPrefix(v, "+") && !strings.HasPrefix(strings.ToLower(v), "joinchat/") {
				v = strings.ToLower(v)
			}
			add("telegram_link", "t.me/"+v)
		}
	}
	if strings.Contains(text, "@") {
		for _, m := range ixReMent.FindAllStringSubmatch(text, -1) {
			add("mention", "@"+strings.ToLower(m[1]))
		}
		for _, m := range ixReEmail.FindAllString(text, -1) {
			add("email", strings.ToLower(m))
		}
	}
	if strings.Contains(text, "+") {
		for _, m := range ixRePhone.FindAllString(text, -1) {
			digits := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, m)
			if len(digits) >= 8 && len(digits) <= 15 {
				add("phone", "+"+digits)
			}
		}
	}
	for _, m := range ixReB58.FindAllString(text, -1) {
		if v, ok := ixBase58Check(m); ok {
			switch {
			case v == 0x00 || v == 0x05:
				add("btc", m)
			case v == 0x30 || v == 0x32:
				add("ltc", m)
			case v == 0x1e || v == 0x16:
				add("doge", m)
			case v == 0x41:
				add("tron", m)
			}
		}
	}
	for _, m := range ixReBech.FindAllString(text, -1) {
		if ixBech32(m) {
			kind := "btc"
			if strings.HasPrefix(strings.ToLower(m), "ltc1") {
				kind = "ltc"
			}
			add(kind, strings.ToLower(m))
		}
	}
	if strings.Contains(text, "0x") {
		for _, m := range ixReEth.FindAllString(text, -1) {
			if ixEIP55(m) {
				add("eth", strings.ToLower(m))
			}
		}
	}
	for _, m := range ixReXmr.FindAllString(text, -1) {
		add("xmr", m)
	}
	for _, m := range ixReIBAN.FindAllString(text, -1) {
		if ixIBAN(m) {
			add("iban", strings.ReplaceAll(m, " ", ""))
		}
	}
	return out
}

// ---------- the entity list ----------

type ixEntityBox struct {
	mu       sync.Mutex
	list     []*ixEntity
	stamp    string
	built    time.Time
	building bool
}

var ixEntityState sync.Map

func (a *App) ixEntityBox() *ixEntityBox {
	v, _ := ixEntityState.LoadOrStore(a, &ixEntityBox{})
	return v.(*ixEntityBox)
}

// ixEntities returns every entity found in archived text, most mentioned
// first, rebuilding the list when messages changed.
func (a *App) ixEntities(ctx context.Context) ([]*ixEntity, error) {
	box := a.ixEntityBox()
	var stamp string
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid),0)||'/'||COUNT(*)||'/'||COALESCE(MAX(seen_at),0) FROM ix_messages`).Scan(&stamp)
	box.mu.Lock()
	if box.list != nil && box.stamp == stamp {
		list := box.list
		box.mu.Unlock()
		return list, nil
	}
	box.mu.Unlock()
	byKey := map[string]*ixEntity{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, message_id, date, text FROM ix_messages WHERE text<>'' ORDER BY date, group_id, message_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref ixEntityRef
		var text string
		if rows.Scan(&ref.Group, &ref.Msg, &ref.Date, &text) != nil {
			continue
		}
		seen := map[string]bool{}
		for _, kv := range ixExtract(text) {
			k := kv[0] + "\x00" + kv[1]
			if seen[k] {
				continue
			}
			seen[k] = true
			e := byKey[k]
			if e == nil {
				e = &ixEntity{Kind: kv[0], Value: kv[1], First: ref.Date, chats: map[string]bool{}}
				byKey[k] = e
			}
			e.Count++
			e.chats[ref.Group] = true
			if ref.Date > e.Last {
				e.Last = ref.Date
			}
			if len(e.Refs) < ixEntityRefs {
				e.Refs = append(e.Refs, ref)
			}
		}
		if ctx.Err() != nil {
			rows.Close()
			return nil, ctx.Err()
		}
	}
	rows.Close()
	list := make([]*ixEntity, 0, len(byKey))
	for _, e := range byKey {
		e.Chats = len(e.chats)
		e.chats = nil
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Count != list[j].Count {
			return list[i].Count > list[j].Count
		}
		if list[i].Kind != list[j].Kind {
			return list[i].Kind < list[j].Kind
		}
		return list[i].Value < list[j].Value
	})
	box.mu.Lock()
	box.list, box.stamp, box.built = list, stamp, time.Now()
	box.mu.Unlock()
	return list, nil
}

// ixEntityCounts counts entities by kind.
func ixEntityCounts(list []*ixEntity) map[string]int {
	out := map[string]int{}
	for _, e := range list {
		out[e.Kind]++
	}
	return out
}

func ixRefKey(group string, msg int) string { return group + ":" + strconv.Itoa(msg) }

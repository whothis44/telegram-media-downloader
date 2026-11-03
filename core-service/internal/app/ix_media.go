package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "image/gif"
	_ "image/png"
)

// The picture index: every unique picture file is read once, at its own
// pace, and from that one read come its SHA-256 (when the downloader had
// none), a grid thumbnail, and its PDQ hash. Pictures are then grouped with
// the first picture they match ("leader clustering"): within 20 bits of a
// leader they join it; 21–31 bits apart they join only if the shapes agree
// and the small grey copies look alike (SSIM), and otherwise the pair is
// put up for review. Each member is close to its leader, so a chain of
// look-alikes cannot pull unrelated pictures together.

const (
	ixPDQAuto      = 20 // bits: the same picture
	ixPDQMatch     = 31 // bits: possibly the same picture, checked further
	ixPDQMinQual   = 50 // hashes of flat or blank pictures are not trusted
	ixSSIMSame     = 0.90
	ixAspectSlack  = 0.02
	ixMaxPictureMB = 40
)

type ixLeader struct {
	key    string
	h      ixPDQ
	luma   []byte
	aspect float64
}

type ixMediaStatus struct {
	Running   bool   `json:"running"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	Errors    int    `json:"errors"`
	Clusters  int    `json:"clusters"`
	Grouped   int    `json:"grouped"` // pictures that joined another
	Suggested int    `json:"suggested"`
	Current   string `json:"current,omitempty"`
}

type ixMediaBox struct {
	mu      sync.Mutex
	st      ixMediaStatus
	leaders []ixLeader
	loaded  bool
	after   int64 // the last download ID looked at in this pass
}

var ixMediaState sync.Map // *App -> *ixMediaBox

func (a *App) ixMediaBox() *ixMediaBox {
	v, _ := ixMediaState.LoadOrStore(a, &ixMediaBox{})
	return v.(*ixMediaBox)
}

func (a *App) ixThumbDir() string { return filepath.Join(a.dataDir, "ixthumbs") }

// ixThumbName is where a file's grid thumbnail is kept, by its key.
func ixThumbName(key string) string {
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:16])
	if strings.HasPrefix(key, "h:") && len(key) == 66 {
		name = key[2:]
	}
	return name[:2] + "/" + name + ".jpg"
}

const ixPictureWhere = `d.message_id>0 AND d.file_path IS NOT NULL AND d.file_path<>'' AND (d.status IS NULL OR d.status='completed')
	AND (d.file_type='photo' OR (d.telegram_mime LIKE 'image/%' AND d.telegram_mime NOT IN ('image/webp','image/heic','image/heif','image/avif','image/svg+xml')))`

func (a *App) ixRunMedia(ctx context.Context) {
	wait := 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = time.Minute
		if !ixVdupBackground.Load() {
			continue
		}
		s := a.ixLoadSettings(ctx)
		if !s.DupEnabled || ixInQuietHours(ixSettings{QuietStart: s.DupQuietStart, QuietEnd: s.DupQuietEnd}, time.Now()) {
			a.ixMediaCount(ctx)
			continue
		}
		for n := 0; n < 500 && ctx.Err() == nil; n++ {
			worked, err := a.ixMediaStep(ctx)
			if err != nil && ctx.Err() == nil {
				a.ixLog("picture index: %v", err)
				break
			}
			if !worked {
				break
			}
			wait = time.Second
		}
		a.ixMediaCount(ctx)
	}
}

// ixMediaStep indexes one picture file; false when there is none left.
func (a *App) ixMediaStep(ctx context.Context) (bool, error) {
	var rel, hash string
	var id int64
	box := a.ixMediaBox()
	box.mu.Lock()
	after := box.after
	box.mu.Unlock()
	// Walk the downloads in ID order from where the last step stopped, so a
	// large library is not scanned from the start for every file; at the end,
	// one more pass from the start picks up anything changed behind it.
	err := a.db.Reader.QueryRowContext(ctx, `SELECT d.id, d.file_path, COALESCE(d.file_hash,'') FROM downloads d WHERE d.id>? AND `+ixPictureWhere+`
		AND NOT EXISTS (SELECT 1 FROM ix_pdq p WHERE p.key = CASE WHEN length(d.file_hash)=64 THEN 'h:'||lower(d.file_hash) ELSE 'p:'||replace(d.file_path,char(92),'/') END)
		ORDER BY d.id LIMIT 1`, after).Scan(&id, &rel, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		box.mu.Lock()
		box.after = 0
		box.mu.Unlock()
		if after > 0 {
			return a.ixMediaStep(ctx)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	box.mu.Lock()
	box.after = id
	box.mu.Unlock()
	stored := rel
	rel = ixNormPath(rel)
	key := ixFileKey(hash, rel)
	box.mu.Lock()
	box.st.Running, box.st.Current = true, rel
	box.mu.Unlock()
	defer func() {
		box.mu.Lock()
		box.st.Running, box.st.Current = false, ""
		box.mu.Unlock()
	}()
	if rel == "" {
		return true, a.ixPDQFail(ctx, key, stored, "bad path")
	}
	full := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
	f, err := os.Open(full)
	if err != nil {
		if _, rootErr := os.Stat(a.downloadsDir); rootErr != nil {
			return false, nil // the NAS is offline: try later
		}
		return true, a.ixPDQFail(ctx, key, rel, "file not found")
	}
	data, err := io.ReadAll(io.LimitReader(f, ixMaxPictureMB<<20+1))
	f.Close()
	if err != nil {
		return true, a.ixPDQFail(ctx, key, rel, err.Error())
	}
	if len(data) > ixMaxPictureMB<<20 {
		return true, a.ixPDQFail(ctx, key, rel, "too large to index")
	}
	if len(hash) != 64 {
		sum := sha256.Sum256(data)
		hash = hex.EncodeToString(sum[:])
		if _, err := a.db.Writer.ExecContext(ctx, `UPDATE downloads SET file_hash=? WHERE file_path=? AND (file_hash IS NULL OR file_hash='')`, hash, stored); err != nil {
			return true, err
		}
		key = "h:" + hash
	}
	pic, err := ixReadPicture(data)
	if err != nil {
		return true, a.ixPDQFail(ctx, key, rel, "not a readable picture: "+err.Error())
	}
	h, quality, buf := ixPDQHash(pic.Luma, pic.Rows, pic.Cols)
	luma := ixLuma32(buf)
	thumb := ""
	if len(pic.Thumb) > 0 {
		name := ixThumbName(key)
		path := filepath.Join(a.ixThumbDir(), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			if err := os.WriteFile(path, pic.Thumb, 0o644); err == nil {
				thumb = name
			}
		}
	}
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO ix_pdq(key,file_path,hash,quality,width,height,luma,thumb,status,error,updated_at) VALUES(?,?,?,?,?,?,?,?,'ok','',?)
		ON CONFLICT(key) DO UPDATE SET file_path=excluded.file_path, hash=excluded.hash, quality=excluded.quality, width=excluded.width, height=excluded.height,
		luma=excluded.luma, thumb=excluded.thumb, status='ok', error='', updated_at=excluded.updated_at`,
		key, rel, h.Bytes(), quality, pic.Width, pic.Height, luma, thumb, time.Now().UnixMilli()); err != nil {
		return true, err
	}
	if err = a.ixClusterPicture(ctx, tx, key, h, quality, luma, float64(pic.Width)/float64(pic.Height)); err != nil {
		return true, err
	}
	return true, tx.Commit()
}

func (a *App) ixPDQFail(ctx context.Context, key, rel, why string) error {
	_, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_pdq(key,file_path,status,error,updated_at) VALUES(?,?,'error',?,?)
		ON CONFLICT(key) DO UPDATE SET status='error', error=excluded.error, updated_at=excluded.updated_at`, key, rel, why, time.Now().UnixMilli())
	return err
}

// ixLoadLeaders reads the cluster leaders once per run.
func (a *App) ixLoadLeaders(ctx context.Context, box *ixMediaBox) error {
	if box.loaded {
		return nil
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT p.key, p.hash, p.luma, p.width, p.height FROM ix_mcl m JOIN ix_pdq p ON p.key=m.key WHERE m.how='leader' AND p.status='ok' ORDER BY p.updated_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	box.leaders = box.leaders[:0]
	for rows.Next() {
		var l ixLeader
		var hb []byte
		var w, h int
		if rows.Scan(&l.key, &hb, &l.luma, &w, &h) != nil {
			continue
		}
		var ok bool
		if l.h, ok = ixPDQFromBytes(hb); !ok || h == 0 {
			continue
		}
		l.aspect = float64(w) / float64(h)
		box.leaders = append(box.leaders, l)
	}
	box.loaded = true
	return rows.Err()
}

// ixPictureVerdict decides a picture against one leader: "auto", "verified",
// "suggest" (close, but the checks disagree) or "" (not the same).
func ixPictureVerdict(d int, aspectA, aspectB float64, lumaA, lumaB []byte) string {
	switch {
	case d <= ixPDQAuto:
		return "auto"
	case d > ixPDQMatch:
		return ""
	}
	if aspectA <= 0 || aspectB <= 0 || math.Abs(aspectA-aspectB)/math.Max(aspectA, aspectB) > ixAspectSlack {
		return "suggest"
	}
	if ixSSIM(lumaA, lumaB, 32) < ixSSIMSame {
		return "suggest"
	}
	return "verified"
}

func (a *App) ixClusterPicture(ctx context.Context, tx *sql.Tx, key string, h ixPDQ, quality int, luma []byte, aspect float64) error {
	box := a.ixMediaBox()
	box.mu.Lock()
	defer box.mu.Unlock()
	if err := a.ixLoadLeaders(ctx, box); err != nil {
		return err
	}
	if quality < ixPDQMinQual {
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_mcl(key,cluster,how,distance) VALUES(?,?,'lowq',0)`, key, key)
		return err
	}
	different := map[string]bool{}
	if rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN key_a=? THEN key_b ELSE key_a END FROM ix_org_pairs WHERE decision='different' AND (key_a=? OR key_b=?)`, key, key, key); err == nil {
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				different[k] = true
			}
		}
		rows.Close()
	}
	best, bestD := -1, ixPDQMatch+1
	for i := range box.leaders {
		if d := h.Distance(box.leaders[i].h); d < bestD && box.leaders[i].key != key && !different[box.leaders[i].key] {
			best, bestD = i, d
		}
	}
	verdict := ""
	if best >= 0 {
		l := box.leaders[best]
		verdict = ixPictureVerdict(bestD, aspect, l.aspect, luma, l.luma)
		if verdict == "suggest" {
			ka, kb := key, l.key
			if kb < ka {
				ka, kb = kb, ka
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ix_msug(key_a,key_b,kind,distance,found_at) VALUES(?,?,'photo',?,?)`, ka, kb, bestD, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
	}
	if verdict == "auto" || verdict == "verified" {
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_mcl(key,cluster,how,distance) VALUES(?,?,?,?)`, key, box.leaders[best].key, verdict, bestD)
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_mcl(key,cluster,how,distance) VALUES(?,?,'leader',0)`, key, key); err != nil {
		return err
	}
	box.leaders = append(box.leaders, ixLeader{key: key, h: h, luma: luma, aspect: aspect})
	return nil
}

func (a *App) ixMediaCount(ctx context.Context) {
	var st ixMediaStatus
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(DISTINCT CASE WHEN length(d.file_hash)=64 THEN 'h:'||lower(d.file_hash) ELSE 'p:'||replace(d.file_path,char(92),'/') END) FROM downloads d WHERE `+ixPictureWhere).Scan(&st.Total)
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(status='error'),0) FROM ix_pdq`).Scan(&st.Done, &st.Errors)
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(how IN ('leader','lowq')),0), COALESCE(SUM(how IN ('auto','verified')),0) FROM ix_mcl`).Scan(&st.Clusters, &st.Grouped)
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_msug s WHERE NOT EXISTS (SELECT 1 FROM ix_org_pairs o WHERE o.key_a=s.key_a AND o.key_b=s.key_b)`).Scan(&st.Suggested)
	box := a.ixMediaBox()
	box.mu.Lock()
	st.Running, st.Current = box.st.Running, box.st.Current
	st.Done = min(st.Done, max(st.Total, st.Done))
	box.st = st
	box.mu.Unlock()
}

func (a *App) ixMediaStatus() ixMediaStatus {
	box := a.ixMediaBox()
	box.mu.Lock()
	defer box.mu.Unlock()
	return box.st
}

// handleIxThumb serves a picture's grid thumbnail by file key.
func (a *App) handleIxThumb(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeNotFound(w, r)
		return
	}
	path := filepath.Join(a.ixThumbDir(), filepath.FromSlash(ixThumbName(key)))
	data, err := os.ReadFile(path)
	if err != nil {
		writeNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=2592000")
	_, _ = w.Write(data)
}

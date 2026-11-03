package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Organising the archive without changing it: collections, review
// decisions on look-alike files, the copy to show for a set of duplicates.
// All of it is kept in the database (backed up with it) and also appended
// to organisation.jsonl in the data folder, one line per change, so it
// survives anything and can be read by other tools.
//
// Moving files is separate and deliberate: a plan lists every operation, a
// person reviews it, and applying it records each step before and after
// (a journal) and checks each file's SHA-256 at its destination before the
// source is let go. "Delete" only ever means moving into a dated _trash
// folder on the same share, and a plan can be undone step by step.

var ixOrgLogMu sync.Mutex

func (a *App) ixOrgLog(event string, fields map[string]any) {
	fields["event"] = event
	fields["at"] = time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(fields)
	if err != nil {
		return
	}
	ixOrgLogMu.Lock()
	defer ixOrgLogMu.Unlock()
	f, err := os.OpenFile(filepath.Join(a.dataDir, "organisation.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// ---------- collections ----------

type ixCollection struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Items   int    `json:"items"`
	Created int64  `json:"created"`
}

func (a *App) ixCollections(ctx context.Context) ([]ixCollection, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT c.id, c.name, c.created_at, (SELECT COUNT(*) FROM ix_org_items i WHERE i.collection_id=c.id) FROM ix_org_collections c ORDER BY c.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ixCollection{}
	for rows.Next() {
		var c ixCollection
		if rows.Scan(&c.ID, &c.Name, &c.Created, &c.Items) == nil {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

func (a *App) ixCreateCollection(ctx context.Context, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return 0, errors.New("a collection needs a name (up to 200 characters)")
	}
	res, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_org_collections(name,created_at) VALUES(?,?)`, name, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	a.ixOrgLog("collection_created", map[string]any{"collection": id, "name": name})
	return id, nil
}

func (a *App) ixRenameCollection(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("a collection needs a name")
	}
	if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_org_collections SET name=? WHERE id=?`, name, id); err != nil {
		return err
	}
	a.ixOrgLog("collection_renamed", map[string]any{"collection": id, "name": name})
	return nil
}

func (a *App) ixDeleteCollection(ctx context.Context, id int64) error {
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM ix_org_items WHERE collection_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM ix_org_collections WHERE id=?`, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.ixOrgLog("collection_deleted", map[string]any{"collection": id})
	return nil
}

// ixValidRef accepts post:<group>:<message> and file:<key>.
func ixValidRef(ref string) bool {
	switch {
	case strings.HasPrefix(ref, "post:"):
		parts := strings.Split(ref, ":")
		if len(parts) != 3 {
			return false
		}
		_, err := strconv.Atoi(parts[2])
		return err == nil && parts[1] != ""
	case strings.HasPrefix(ref, "file:"):
		return len(ref) > 7
	}
	return false
}

func (a *App) ixCollect(ctx context.Context, id int64, refs []string, remove bool) error {
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_org_collections WHERE id=?`, id).Scan(&exists); err != nil || exists == 0 {
		return errors.New("no such collection")
	}
	now := time.Now().UnixMilli()
	for _, ref := range refs {
		if !ixValidRef(ref) {
			return fmt.Errorf("bad item %q", ref)
		}
		if remove {
			_, err = tx.ExecContext(ctx, `DELETE FROM ix_org_items WHERE collection_id=? AND ref=?`, id, ref)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ix_org_items(collection_id,ref,added_at) VALUES(?,?,?)`, id, ref, now)
		}
		if err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	event := "collection_added"
	if remove {
		event = "collection_removed"
	}
	a.ixOrgLog(event, map[string]any{"collection": id, "items": refs})
	return nil
}

func (a *App) ixCollectionRefs(ctx context.Context, id int64) ([]string, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT ref FROM ix_org_items WHERE collection_id=? ORDER BY added_at DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if rows.Scan(&r) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// ---------- review decisions ----------

// ixDecide records whether two files are the same picture or video.
func (a *App) ixDecide(ctx context.Context, ka, kb, decision string) error {
	if decision != "same" && decision != "different" {
		return errors.New("decision must be same or different")
	}
	if ka == "" || kb == "" || ka == kb {
		return errors.New("two different files are needed")
	}
	if kb < ka {
		ka, kb = kb, ka
	}
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_org_pairs(key_a,key_b,decision,decided_at) VALUES(?,?,?,?)
		ON CONFLICT(key_a,key_b) DO UPDATE SET decision=excluded.decision, decided_at=excluded.decided_at`, ka, kb, decision, time.Now().UnixMilli()); err != nil {
		return err
	}
	a.ixOrgLog("pair_decided", map[string]any{"a": ka, "b": kb, "decision": decision})
	return nil
}

// ixSetKeeper chooses the copy shown for a set of duplicates; the cluster
// is named by any of its files.
func (a *App) ixSetKeeper(ctx context.Context, clusterKey, key string) error {
	if clusterKey == "" || key == "" {
		return errors.New("a set and a file are needed")
	}
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_org_keeper(cluster,key) VALUES(?,?) ON CONFLICT(cluster) DO UPDATE SET key=excluded.key`, clusterKey, key); err != nil {
		return err
	}
	a.ixOrgLog("keeper_chosen", map[string]any{"set": clusterKey, "file": key})
	return nil
}

// ---------- file moves ----------

type ixMoveOp struct {
	Seq     int    `json:"seq"`
	Op      string `json:"op"` // copy, move, trash
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	SHA256  string `json:"sha256,omitempty"`
	Size    int64  `json:"size"`
	Repoint string `json:"repoint,omitempty"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

type ixMovePlan struct {
	ID        int64      `json:"id"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	Created   int64      `json:"created"`
	AppliedAt int64      `json:"appliedAt,omitempty"`
	UndoneAt  int64      `json:"undoneAt,omitempty"`
	Error     string     `json:"error,omitempty"`
	Ops       int        `json:"ops"`
	Bytes     int64      `json:"bytes"`
	Done      int        `json:"done"`
	List      []ixMoveOp `json:"list,omitempty"`
}

const ixMaxPlanOps = 100000

var ixMoveMu sync.Mutex // one plan applies at a time

// ixPathKey compares stored paths: separators and "./" normalised, and
// letter case ignored on Windows, where the share does too.
func ixPathKey(p string) string {
	n := ixNormPath(p)
	if runtime.GOOS == "windows" {
		n = strings.ToLower(n)
	}
	return n
}

// ixPlanDuplicateCopies plans moving byte-identical extra copies (same
// SHA-256, stored at different paths) to _trash, keeping the copy saved
// first; every catalogue entry of a trashed copy points at the kept one.
// Two spellings of one path, or two names for one file on disk, are never
// taken for two copies.
func (a *App) ixPlanDuplicateCopies(ctx context.Context) ([]ixMoveOp, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT lower(file_hash), file_path, MIN(id), MAX(COALESCE(file_size,0)) FROM downloads
		WHERE length(file_hash)=64 AND file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed')
		AND lower(file_hash) IN (SELECT lower(file_hash) FROM downloads WHERE length(file_hash)=64 AND file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed') GROUP BY lower(file_hash) HAVING COUNT(DISTINCT file_path)>1)
		GROUP BY lower(file_hash), file_path ORDER BY lower(file_hash), MIN(id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stamp := time.Now().In(ixTZ).Format("2006-01-02")
	type kept struct {
		stored, rel string
		info        os.FileInfo
	}
	var ops []ixMoveOp
	keep := map[string]kept{}
	seen := map[string]bool{} // hash + path key
	for rows.Next() {
		var sum, path string
		var first, size int64
		if rows.Scan(&sum, &path, &first, &size) != nil {
			continue
		}
		rel := ixNormPath(path)
		if rel == "" || seen[sum+"\x00"+ixPathKey(path)] {
			continue
		}
		seen[sum+"\x00"+ixPathKey(path)] = true
		info, err := os.Stat(a.ixAbs(rel))
		if err != nil || !info.Mode().IsRegular() {
			continue // not on disk: neither a copy to move nor one to keep
		}
		k, ok := keep[sum]
		if !ok {
			keep[sum] = kept{path, rel, info}
			continue
		}
		if os.SameFile(info, k.info) {
			continue // another name for the kept file itself
		}
		ops = append(ops, ixMoveOp{Op: "trash", Src: rel, Dst: "_trash/" + stamp + "/" + rel, SHA256: sum, Size: size, Repoint: k.stored})
		if len(ops) >= ixMaxPlanOps {
			break
		}
	}
	return ops, rows.Err()
}

// ixPlanCopy plans copying (or moving) the given files into a folder,
// named by date, chat and message so they sort as they were posted.
func (a *App) ixPlanCopy(ctx context.Context, m *ixModel, items []int32, folder string, move bool) ([]ixMoveOp, error) {
	folder = strings.Trim(strings.ReplaceAll(folder, `\`, "/"), "/")
	dest := ixNormPath(folder)
	if dest == "" || strings.HasPrefix(dest, "_trash") {
		return nil, errors.New("choose a folder inside the downloads folder (for example: collections/my-set)")
	}
	op := "copy"
	if move {
		op = "move"
	}
	var ops []ixMoveOp
	used := map[string]bool{}
	for _, ii := range items {
		it := &m.Items[ii]
		if it.Key == "" || it.Path == "" {
			continue
		}
		p := &m.Posts[it.Post]
		date := time.Unix(p.Date, 0).In(ixTZ).Format("2006-01-02_1504")
		chat := ixSlug(m.GroupName[it.Group])
		if chat == "" {
			chat = ixBare(it.Group)
		}
		name := fmt.Sprintf("%s_%s_%d_%s", date, chat, it.Msg, filepath.Base(it.Path))
		dst := dest + "/" + name
		for n := 2; used[dst]; n++ {
			dst = dest + "/" + strconv.Itoa(n) + "_" + name
		}
		used[dst] = true
		sum := ""
		if strings.HasPrefix(it.Key, "h:") {
			sum = it.Key[2:]
		}
		o := ixMoveOp{Op: op, Src: it.Path, Dst: dst, SHA256: sum, Size: it.Size}
		if move {
			o.Repoint = dst
		}
		ops = append(ops, o)
		if len(ops) >= ixMaxPlanOps {
			break
		}
	}
	if len(ops) == 0 {
		return nil, errors.New("nothing to copy: none of these files is downloaded")
	}
	return ops, nil
}

func (a *App) ixSavePlan(ctx context.Context, kind, title string, ops []ixMoveOp) (int64, error) {
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO ix_move_plans(kind,title,state,created_at) VALUES(?,?,'planned',?)`, kind, title, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	for i, o := range ops {
		if _, err = tx.ExecContext(ctx, `INSERT INTO ix_moves(plan_id,seq,op,src,dst,sha256,size,repoint,state) VALUES(?,?,?,?,?,?,?,?,'planned')`,
			id, i+1, o.Op, o.Src, o.Dst, o.SHA256, o.Size, o.Repoint); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	a.ixOrgLog("move_planned", map[string]any{"plan": id, "kind": kind, "title": title, "ops": len(ops)})
	return id, nil
}

func (a *App) ixPlans(ctx context.Context) ([]ixMovePlan, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT p.id, p.kind, p.title, p.state, p.created_at, p.applied_at, p.undone_at, p.error,
		(SELECT COUNT(*) FROM ix_moves m WHERE m.plan_id=p.id), (SELECT COALESCE(SUM(size),0) FROM ix_moves m WHERE m.plan_id=p.id),
		(SELECT COUNT(*) FROM ix_moves m WHERE m.plan_id=p.id AND m.state='done')
		FROM ix_move_plans p ORDER BY p.id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ixMovePlan{}
	for rows.Next() {
		var p ixMovePlan
		if rows.Scan(&p.ID, &p.Kind, &p.Title, &p.State, &p.Created, &p.AppliedAt, &p.UndoneAt, &p.Error, &p.Ops, &p.Bytes, &p.Done) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func (a *App) ixPlan(ctx context.Context, id int64, limit int) (ixMovePlan, error) {
	var p ixMovePlan
	err := a.db.Reader.QueryRowContext(ctx, `SELECT id, kind, title, state, created_at, applied_at, undone_at, error FROM ix_move_plans WHERE id=?`, id).
		Scan(&p.ID, &p.Kind, &p.Title, &p.State, &p.Created, &p.AppliedAt, &p.UndoneAt, &p.Error)
	if err != nil {
		return p, err
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT seq, op, src, dst, sha256, size, repoint, state, error FROM ix_moves WHERE plan_id=? ORDER BY seq`, id)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var o ixMoveOp
		if rows.Scan(&o.Seq, &o.Op, &o.Src, &o.Dst, &o.SHA256, &o.Size, &o.Repoint, &o.State, &o.Error) != nil {
			continue
		}
		p.Ops++
		p.Bytes += o.Size
		if o.State == "done" {
			p.Done++
		}
		if limit <= 0 || len(p.List) < limit {
			p.List = append(p.List, o)
		}
	}
	return p, rows.Err()
}

func (a *App) ixAbs(rel string) string {
	return filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
}

func ixHashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ixCopyVerified copies src to dst through a temporary name and checks the
// copy's SHA-256 against want (or the source's own).
func ixCopyVerified(src, dst, want string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".ixpart"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if want != "" && !strings.EqualFold(got, want) {
		_ = os.Remove(tmp)
		return fmt.Errorf("the source's SHA-256 is %s, not the recorded %s", got, want)
	}
	if again, _, err := ixHashFile(tmp); err != nil || again != got {
		_ = os.Remove(tmp)
		return errors.New("the copy does not read back the same")
	}
	return os.Rename(tmp, dst)
}

// ixMoveVerified moves src to dst: a rename on the same share, otherwise a
// verified copy and then removal of the source.
func ixMoveVerified(src, dst, want string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	if want != "" {
		got, _, err := ixHashFile(src)
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("the file's SHA-256 is %s, not the recorded %s", got, want)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := ixCopyVerified(src, dst, want); err != nil {
		return err
	}
	return os.Remove(src)
}

// ixLanded says whether a move's file is already at its destination (an
// earlier attempt stopped after moving it, before recording that).
func (a *App) ixLanded(o ixMoveOp) bool {
	if _, err := os.Stat(a.ixAbs(o.Src)); !os.IsNotExist(err) {
		return false
	}
	got, _, err := ixHashFile(a.ixAbs(o.Dst))
	return err == nil && (o.SHA256 == "" || strings.EqualFold(got, o.SHA256))
}

// ixCheckKeeper makes sure the copy a trashed duplicate's entries will point
// at is there, is a different file, and has the same bytes.
func (a *App) ixCheckKeeper(o ixMoveOp) error {
	keepRel := ixNormPath(o.Repoint)
	if keepRel == "" || ixPathKey(keepRel) == ixPathKey(o.Src) {
		return errors.New("the copy to keep is this same file")
	}
	ki, err := os.Stat(a.ixAbs(keepRel))
	if err != nil {
		return fmt.Errorf("the copy to keep is missing: %w", err)
	}
	if si, err := os.Stat(a.ixAbs(o.Src)); err == nil && os.SameFile(si, ki) {
		return errors.New("the copy to keep is this same file")
	}
	got, _, err := ixHashFile(a.ixAbs(keepRel))
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, o.SHA256) {
		return errors.New("the copy to keep no longer has the same bytes")
	}
	return nil
}

// ixApplyPlan carries out a plan, journalling every step.
func (a *App) ixApplyPlan(ctx context.Context, id int64) error {
	ixMoveMu.Lock()
	defer ixMoveMu.Unlock()
	p, err := a.ixPlan(ctx, id, 0)
	if err != nil {
		return err
	}
	if p.State != "planned" && p.State != "failed" {
		return fmt.Errorf("this plan is %s", p.State)
	}
	if _, err := os.Stat(a.downloadsDir); err != nil {
		return errors.New("the downloads folder is not reachable")
	}
	_, _ = a.db.Writer.ExecContext(ctx, `UPDATE ix_move_plans SET state='applying', error='' WHERE id=?`, id)
	a.ixOrgLog("move_applying", map[string]any{"plan": id})
	var failure error
	for _, o := range p.List {
		if o.State == "done" {
			continue
		}
		if err := ctx.Err(); err != nil {
			failure = err
			break
		}
		_, _ = a.db.Writer.ExecContext(ctx, `UPDATE ix_moves SET state='running', at=? WHERE plan_id=? AND seq=?`, time.Now().UnixMilli(), id, o.Seq)
		var opErr error
		moved := false
		switch o.Op {
		case "copy":
			opErr = ixCopyVerified(a.ixAbs(o.Src), a.ixAbs(o.Dst), o.SHA256)
		case "move", "trash":
			if o.Op == "trash" {
				opErr = a.ixCheckKeeper(o)
			}
			if opErr == nil {
				if a.ixLanded(o) {
					moved = true // an earlier attempt got this far
				} else if opErr = ixMoveVerified(a.ixAbs(o.Src), a.ixAbs(o.Dst), o.SHA256); opErr == nil {
					moved = true
				}
			}
		default:
			opErr = fmt.Errorf("unknown operation %q", o.Op)
		}
		if opErr == nil && o.Repoint != "" {
			if opErr = a.ixRepoint(ctx, id, o); opErr != nil && moved {
				// The catalogue could not follow: put the file back.
				if back := ixMoveVerified(a.ixAbs(o.Dst), a.ixAbs(o.Src), o.SHA256); back != nil {
					opErr = fmt.Errorf("%v; moving it back also failed: %v (it is at %s)", opErr, back, o.Dst)
				}
			}
		}
		state, msg := "done", ""
		if opErr != nil {
			state, msg = "failed", opErr.Error()
			failure = opErr
		}
		_, _ = a.db.Writer.ExecContext(ctx, `UPDATE ix_moves SET state=?, error=?, at=? WHERE plan_id=? AND seq=?`, state, msg, time.Now().UnixMilli(), id, o.Seq)
		if failure != nil {
			break
		}
	}
	state, msg := "applied", ""
	if failure != nil {
		state, msg = "failed", failure.Error()
	}
	_, _ = a.db.Writer.ExecContext(ctx, `UPDATE ix_move_plans SET state=?, error=?, applied_at=? WHERE id=?`, state, msg, time.Now().UnixMilli(), id)
	a.ixOrgLog("move_"+state, map[string]any{"plan": id, "error": msg})
	return failure
}

// ixUndoPlan reverses the steps of a plan, newest first: those recorded as
// done, and any that moved a file before an interruption.
func (a *App) ixUndoPlan(ctx context.Context, id int64) error {
	ixMoveMu.Lock()
	defer ixMoveMu.Unlock()
	p, err := a.ixPlan(ctx, id, 0)
	if err != nil {
		return err
	}
	if p.State == "undone" {
		return errors.New("this plan was already undone")
	}
	ops := append([]ixMoveOp(nil), p.List...)
	sort.Slice(ops, func(i, j int) bool { return ops[i].Seq > ops[j].Seq })
	var failure error
	for _, o := range ops {
		landed := (o.State == "running" || o.State == "failed") && (o.Op == "move" || o.Op == "trash") && a.ixLanded(o)
		if o.State != "done" && !landed {
			continue
		}
		var opErr error
		switch o.Op {
		case "copy":
			if got, _, err := ixHashFile(a.ixAbs(o.Dst)); err == nil && (o.SHA256 == "" || strings.EqualFold(got, o.SHA256)) {
				opErr = os.Remove(a.ixAbs(o.Dst)) // the original is still where it was
			}
		case "move", "trash":
			opErr = ixMoveVerified(a.ixAbs(o.Dst), a.ixAbs(o.Src), o.SHA256)
			if opErr == nil && o.Repoint != "" {
				opErr = a.ixUnrepoint(ctx, id, o)
			}
		}
		if opErr != nil {
			failure = opErr
			_, _ = a.db.Writer.ExecContext(ctx, `UPDATE ix_moves SET error=? WHERE plan_id=? AND seq=?`, "undo: "+opErr.Error(), id, o.Seq)
			break
		}
		_, _ = a.db.Writer.ExecContext(ctx, `UPDATE ix_moves SET state='undone', at=? WHERE plan_id=? AND seq=?`, time.Now().UnixMilli(), id, o.Seq)
	}
	state, msg := "undone", ""
	if failure != nil {
		state, msg = "failed", "undo stopped: "+failure.Error()
	}
	_, _ = a.db.Writer.ExecContext(ctx, `UPDATE ix_move_plans SET state=?, error=?, undone_at=? WHERE id=?`, state, msg, time.Now().UnixMilli(), id)
	a.ixOrgLog("move_"+state, map[string]any{"plan": id, "error": msg})
	return failure
}

// ixRowsAt lists the catalogue entries whose stored path is this file,
// however it is spelled.
func ixRowsAt(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, rel string) ([]string, string, error) {
	base := rel[strings.LastIndex(rel, "/")+1:]
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(base)
	rows, err := q.QueryContext(ctx, `SELECT id, file_path FROM downloads WHERE file_path LIKE ? ESCAPE '\'`, like)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var ids []string
	var stored string
	want := ixPathKey(rel)
	for rows.Next() {
		var id int64
		var p string
		if rows.Scan(&id, &p) == nil && ixPathKey(p) == want {
			ids = append(ids, strconv.FormatInt(id, 10))
			stored = p
		}
	}
	return ids, stored, rows.Err()
}

// ixRepoint points the catalogue entries at a moved file's new place (or,
// for a trashed copy, at the copy kept), recording which entries changed so
// that undoing restores exactly those. A move no entry points at is refused.
func (a *App) ixRepoint(ctx context.Context, plan int64, o ixMoveOp) error {
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ids, original, err := ixRowsAt(ctx, tx, o.Src)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("no catalogue entry points at this file any more")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE downloads SET file_path=? WHERE id IN (`+strings.Join(ids, ",")+`)`, o.Repoint); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ix_moves SET rows=?, src_stored=? WHERE plan_id=? AND seq=?`, strings.Join(ids, ","), original, plan, o.Seq); err != nil {
		return err
	}
	return tx.Commit()
}

// ixUnrepoint points the journalled entries back at the restored file, but
// only those still pointing where the move left them.
func (a *App) ixUnrepoint(ctx context.Context, plan int64, o ixMoveOp) error {
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ids, original string
	if err := tx.QueryRowContext(ctx, `SELECT rows, src_stored FROM ix_moves WHERE plan_id=? AND seq=?`, plan, o.Seq).Scan(&ids, &original); err != nil {
		return err
	}
	if ids == "" {
		return tx.Commit()
	}
	journal := map[string]bool{}
	for _, id := range strings.Split(ids, ",") {
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			return fmt.Errorf("bad journal entry %q", id)
		}
		journal[id] = true
	}
	if original == "" {
		original = o.Src
	}
	at, _, err := ixRowsAt(ctx, tx, o.Repoint)
	if err != nil {
		return err
	}
	var back []string
	for _, id := range at {
		if journal[id] {
			back = append(back, id)
		}
	}
	if len(back) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE downloads SET file_path=? WHERE id IN (`+strings.Join(back, ",")+`)`, original); err != nil {
			return err
		}
	}
	return tx.Commit()
}

package app

import (
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// The forecast to the deletion date (ix_forecast.go), with an invented queue.

func TestIxForecastRates(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := func(ago int, gb float64, premium, secs int) telegram.PaceHour {
		return telegram.PaceHour{Start: now.Add(-time.Duration(ago) * time.Hour).Truncate(time.Hour).Unix(), Bytes: int64(gb * 1e9), Premium: premium, Seconds: secs}
	}
	hours := []telegram.PaceHour{
		h(40, 30, 0, 3600), h(30, 2, 900, 3600), h(29, 2, 900, 3600), // older than a day
		h(10, 3, 600, 3600), h(5, 36, 0, 3600), h(1, 1, 400, 1800),
		h(2, 0.1, 400, 600), // too little measured to be a limited hour
	}
	r := ixForecastRates(hours, now)
	if r[0].Hours != 4 || r[1].Hours != 4 {
		t.Fatalf("hours: %+v", r)
	}
	// Day: 40.1 GB over 9600 s; limited: 8 GB over 12600 s.
	if got := r[0].Rate * 9600; got < 40.09e9 || got > 40.11e9 {
		t.Fatalf("day rate %v", r[0].Rate)
	}
	if got := r[1].Rate * 12600; got < 7.99e9 || got > 8.01e9 {
		t.Fatalf("limited rate %v", r[1].Rate)
	}
	// Fewer than three limited hours: not known.
	if r := ixForecastRates(hours[3:5], now); r[1].Rate != 0 || r[0].Rate == 0 {
		t.Fatalf("few hours: %+v", r)
	}
}

func TestIxForecastOrderAndCutoff(t *testing.T) {
	a, ctx := ixTestApp(t)
	const mb = int64(1) << 20
	add := func(msg int, group string, size int64, identity, status string, sched int, paused int) int64 {
		t.Helper()
		res, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,sched_order,paused,created_at,updated_at)
			VALUES('acc',?,'invented',?,1,?,'video','f.mp4',?,x'00',?,'history',?,?,1,1)`, group, msg, identity, size, status, sched, paused)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	ranked, other := "-1001", "-1002"
	add(1, other, 100*mb, "doc:1", "processing", 131072, 0) // downloading now
	add(2, other, 50*mb, "doc:2", "pending", 131072, 0)     // normal, unranked
	add(3, ranked, 200*mb, "doc:3", "pending", 10, 0)       // ranked: goes before 2
	add(4, other, 50*mb, "doc:3", "pending", 131072, 0)     // same Telegram file as 3: free
	big := add(5, other, 2000*mb, "doc:5", "pending", 131072, 1)
	mustExec(t, a, `INSERT INTO ix_deferred(work_id,band,deferred_at,released_at) VALUES(?,3,1,0)`, big)
	small := add(6, other, 5*mb, "doc:6", "pending", 131072, 1)
	mustExec(t, a, `INSERT INTO ix_deferred(work_id,band,deferred_at,released_at) VALUES(?,0,1,0)`, small) // set aside, smaller band first
	held := add(7, other, 300*mb, "doc:7", "pending", 131072, 1)
	mustExec(t, a, `INSERT INTO ix_held(work_id,group_id,message_id,like_key,reason,held_at) VALUES(?,?,7,'h:k','looks like a saved copy',1)`, held, other)
	add(8, other, 10*mb, "doc:8", "pending", 131072, 1) // paused by a person
	mustExec(t, a, `INSERT INTO tgdl_group_rank(group_id,rank,sched_base,name) VALUES(?,1,0,'invented ranked')`, ranked)

	// 360 MB fit at the 24 h speed (100 + 200 + 50 = 350, then 5 = 355; the
	// 2000 MB file does not), everything at the other.
	saved := ixForecastHours
	t.Cleanup(func() { ixForecastHours = saved })
	ixForecastHours = func(*App) []telegram.PaceHour { return nil }
	until := time.Now().Unix() + 3600
	f := a.ixForecastNow(ctx, ixDeadlineInfo{Until: until})
	if f == nil || f.Rates[0].Rate != 0 || f.Total.DoneFiles[0] != 0 {
		t.Fatalf("no speeds known: %+v", f)
	}
	if f.Paused != 1 || f.Total.Files != 7 || f.Total.Bytes != (100+50+200+2000+5+300)*mb {
		t.Fatalf("totals: paused %d, %+v", f.Paused, f.Total)
	}
	// The last day: 360 MB an hour (an hour left: 360 MB fit); speed-limited
	// hours, older than a day here: a terabyte an hour.
	now := time.Now()
	ixForecastHours = func(*App) []telegram.PaceHour {
		var out []telegram.PaceHour
		for i := 0; i < 4; i++ {
			out = append(out, telegram.PaceHour{Start: now.Add(-time.Duration(i) * time.Hour).Unix(), Bytes: 360 * mb, Seconds: 3600},
				telegram.PaceHour{Start: now.Add(-time.Duration(30+i) * time.Hour).Unix(), Bytes: 1 << 40, Premium: 100, Seconds: 3600})
		}
		return out
	}
	// One more file of 5 MB, not queued yet (6 distinct files are queued).
	f = a.ixForecastNow(ctx, ixDeadlineInfo{Until: until, ToGetFiles: 7, ToGetBytes: (2655 + 5) * mb})
	if f.Unqueued != 1 || f.UnqBytes != 5*mb || f.UnqFit[0] != 0 || f.UnqFit[1] != 5*mb {
		t.Fatalf("not queued yet: %d files, %d bytes, fit %v", f.Unqueued, f.UnqBytes, f.UnqFit)
	}
	st := f.Stages
	if st[ixFcNow].DoneFiles[0] != 1 || st[ixFcRanked].DoneFiles[0] != 1 || st[ixFcOther].Files != 2 || st[ixFcOther].DoneFiles[0] != 2 {
		t.Fatalf("stages: %+v", st)
	}
	if st[ixFcDeferred].Files != 2 || st[ixFcDeferred].DoneFiles[0] != 1 || st[ixFcDeferred].DoneBytes[0] != 5*mb {
		t.Fatalf("set aside: smaller band first, the big one does not fit: %+v", st[ixFcDeferred])
	}
	if st[ixFcHeld].DoneFiles[0] != 0 || st[ixFcHeld].DoneFiles[1] != 1 {
		t.Fatalf("held: %+v", st[ixFcHeld])
	}
	if !f.Rates[1].Finishes || f.Rates[0].Finishes {
		t.Fatalf("finishes: %+v", f.Rates)
	}
	if f.Groups[0].GroupID != ranked || f.Groups[0].Place != 1 || f.Groups[0].DoneFiles[0] != 1 {
		t.Fatalf("groups: %+v", f.Groups)
	}
	if b := f.Bands[4]; b.Files != 1 || b.DoneFiles[0] != 0 {
		t.Fatalf("1 GB band: %+v", b)
	}
	lines := ixForecastDiag(f, func(string) string { return "G1" })
	all := strings.Join(lines, "\n")
	for _, want := range []string{"forecast to the deletion date", "if the speed limit stays on", "by stage", "by size", "place 1 G1", "not counted: 1 paused", "not queued yet: about 1 files"} {
		if !strings.Contains(all, want) {
			t.Fatalf("report lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "invented") {
		t.Fatalf("a name in the report:\n%s", all)
	}
	if ixForecastDiag(nil, nil)[0] != "forecast to the deletion date: no deletion date set" {
		t.Fatal("no deletion date")
	}
	if a.ixForecastNow(ctx, ixDeadlineInfo{}) != nil {
		t.Fatal("a forecast without a deletion date")
	}
}

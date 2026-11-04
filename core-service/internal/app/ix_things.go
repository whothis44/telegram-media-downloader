package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Things in pictures: a list of subjects (editable in Settings), each
// matched against every picture's embedding. A picture shows a subject
// when it is far closer to it than pictures generally are (see outliers),
// which works the same whatever the model's scale. No extra pass over the files: the
// embeddings made for search are used.

const ixDefaultThings = `people
crowd
selfie
document
screenshot
text message
map
chart or graph
meme
cartoon or drawing
logo
flag
car
motorcycle
bicycle
truck
airplane
boat
train
military vehicle
weapon
banknotes
building
street
landscape
mountains
beach
snow
night sky
fire
food
animal
dog
cat
horse
concert
sports
video game`

type ixThing struct {
	Label string  `json:"label"`
	Count int     `json:"count"`
	thr   float32 // likeness at or above which a picture shows it
	vec   []float32
	keys  []string // the files at or above thr (copies of one file included)
}

type ixThingsCache struct {
	mu    sync.Mutex
	stamp string
	list  []ixThing
	text  map[string][]float32 // model|label -> embedding of the label
}

var ixThingsState sync.Map // *App -> *ixThingsCache

func (a *App) ixThingsBox() *ixThingsCache {
	v, _ := ixThingsState.LoadOrStore(a, &ixThingsCache{text: map[string][]float32{}})
	return v.(*ixThingsCache)
}

func ixThingLabels(s ixSettings) []string {
	src := s.MLThings
	if strings.TrimSpace(src) == "" {
		src = ixDefaultThings
	}
	var out []string
	seen := map[string]bool{}
	for _, l := range strings.Split(src, "\n") {
		if l = strings.TrimSpace(l); l != "" && !seen[strings.ToLower(l)] {
			seen[strings.ToLower(l)] = true
			out = append(out, l)
		}
	}
	return out
}

// ixThings matches the subjects against every embedded picture (cached until
// the list, the model or the number of pictures changes noticeably).
func (a *App) ixThings(ctx context.Context) ([]ixThing, error) {
	c := a.ixMLClientNow(ctx)
	if c == nil {
		return nil, errors.New("image recognition is off (Archive → Settings)")
	}
	box := a.ixMLBox()
	if err := box.store.load(ctx, a.db.Reader, c.model); err != nil {
		return nil, err
	}
	labels := ixThingLabels(a.ixLoadSettings(ctx))
	sum := sha256.Sum256([]byte(c.model + "\x00" + strings.Join(labels, "\n")))
	stamp := hex.EncodeToString(sum[:8]) + "/" + strconv.Itoa(box.store.size()/250)
	tc := a.ixThingsBox()
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.stamp == stamp && tc.list != nil {
		return ixThingsCounted(a.ixModelLast(), tc.list), nil
	}
	var list []ixThing
	for _, l := range labels {
		k := c.model + "|" + l
		v := tc.text[k]
		if v == nil {
			var err error
			if v, err = c.embed(ctx, nil, "a photo of "+l); err != nil {
				return nil, err
			}
			tc.text[k] = v
		}
		sc := box.store.scan(v, 4, 0, nil, true)
		list = append(list, ixThing{Label: l, Count: len(sc.above), thr: sc.thr, vec: v, keys: sc.above})
	}
	tc.stamp, tc.list = stamp, list
	return ixThingsCounted(a.ixModelLast(), list), nil
}

// ixThingsCounted counts each subject's files as duplicate sets, by the
// browser's current model (copies of one file count once).
func ixThingsCounted(m *ixModel, list []ixThing) []ixThing {
	out := append([]ixThing(nil), list...)
	for i := range out {
		out[i].Count = ixDistinctFiles(m, out[i].keys)
	}
	return out
}

// outliers says how alike a picture must be to q to stand out, and how
// many do. The background (pictures not showing it) is estimated from the
// lower half of all likenesses (median, and the spread between the first
// quartile and the median), so a subject shown in many pictures does not
// raise its own bar; a picture stands out k estimated deviations above.
func (s *ixEmbStore) outliers(q []float32, k float64) (float32, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dim == 0 || len(q) != s.dim || len(s.keys) < 20 {
		return 2, 0
	}
	qq, qn := ixQuantise(q)
	d := s.dim
	sims := make([]float64, 0, len(s.keys))
	for i := range s.keys {
		if s.norms[i] == 0 {
			continue
		}
		sims = append(sims, float64(ixDot8(qq, s.vecs[i*d:(i+1)*d]))/float64(qn*s.norms[i]))
	}
	thr := ixOutlierThr(sims, k)
	count := 0
	for _, c := range sims {
		if c >= thr {
			count++
		}
	}
	return float32(thr), count
}

// ixOutlierThr is outliers' rule on a list of likenesses (in any order):
// the median plus k deviations, estimated from the first quartile.
func ixOutlierThr(sims []float64, k float64) float64 {
	if len(sims) == 0 {
		return 2
	}
	sorted := append([]float64(nil), sims...)
	sort.Float64s(sorted)
	med, p25 := sorted[len(sorted)/2], sorted[len(sorted)/4]
	sd := math.Max((med-p25)/0.674, 0.01)
	return med + k*sd
}

func (a *App) handleIxThings(w http.ResponseWriter, r *http.Request) {
	list, err := a.ixThings(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := append([]ixThing(nil), list...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	writeJSON(w, http.StatusOK, map[string]any{"things": out})
}

// ixThingHits lists the pictures that show a subject, best first.
func (a *App) ixThingHits(ctx context.Context, label string, limit int, keep func(string) bool) ([]ixEmbHit, error) {
	list, err := a.ixThings(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range list {
		if strings.EqualFold(t.Label, label) {
			var out []ixEmbHit
			var hits []ixEmbHit
			if m := a.ixModelLast(); m != nil {
				hits = ixNearestUnique(m, &a.ixMLBox().store, t.vec, limit, keep) // copies of one file once
			} else {
				hits = a.ixMLBox().store.nearest(t.vec, limit, keep, "")
			}
			for _, h := range hits {
				if h.Sim >= t.thr {
					out = append(out, h)
				}
			}
			return out, nil
		}
	}
	return nil, errors.New("no such subject in the list (Archive → Settings)")
}

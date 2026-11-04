package app

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Why faces are not grouped: numbers only (no files, no people), for the
// diagnostics report and the People tab. They tell which rule keeps faces
// loose on this archive's own content:
//   - unusable: box under 40 px (small) or sharpness under 10 (blurry);
//   - weak, by reason (score, box, sharpness under the start gates; a face
//     may have several), and how many are 0.5-0.6 alike to a grouped face
//     (kept out only by the stricter likeness weak faces need);
//   - good but alone: no other usable face alike enough to be the same
//     person;
//   - good with look-alikes, but in fewer than three files (or fewer than
//     two with a good face);
//   - good and could be grouped now: alike to a person, or look-alikes in
//     enough files (they wait for the next round, or a decision keeps them
//     out: a face taken out, two faces of one picture);
//
// and the spread (10th, 50th, 90th percentile) of score, box and sharpness
// over all faces, with the start gates in use.
type ixFaceLoose struct {
	Faces     int `json:"faces"`
	Loose     int `json:"loose"`
	Small     int `json:"small"`
	Blurry    int `json:"blurry"`
	Weak      int `json:"weak"`
	WeakScore int `json:"weakScore"`
	WeakSize  int `json:"weakSize"`
	WeakSharp int `json:"weakSharp"`
	WeakNear  int `json:"weakNear"`
	Good      int `json:"good"`
	Alone     int `json:"alone"`
	Few       int `json:"few"`
	Ready     int `json:"ready"`
	Sampled   int `json:"sampled,omitempty"` // loose faces looked at when not all were (alone, few, ready, weakNear are scaled up)

	Score [3]float64  `json:"score"` // p10, p50, p90
	Px    [3]float64  `json:"px"`
	Blur  [3]float64  `json:"sharpness"`
	Gates ixFaceGates `json:"gates"`
}

const ixFaceLooseSample = 2000

type ixFaceLooseBox struct {
	mu   sync.Mutex
	last *ixFaceLoose
	at   time.Time
	run  chan struct{}
}

var ixFaceLooseState sync.Map // *App -> *ixFaceLooseBox

// ixFaceLooseNow gives the numbers, worked out again (in the background)
// when older than maxAge; it waits up to wait for them.
func (a *App) ixFaceLooseNow(ctx context.Context, maxAge, wait time.Duration) *ixFaceLoose {
	v, _ := ixFaceLooseState.LoadOrStore(a, &ixFaceLooseBox{})
	lb := v.(*ixFaceLooseBox)
	lb.mu.Lock()
	if lb.last != nil && time.Since(lb.at) < maxAge {
		defer lb.mu.Unlock()
		return lb.last
	}
	if lb.run == nil {
		ch := make(chan struct{})
		lb.run = ch
		base := a.ctx
		if base == nil {
			base = context.Background()
		}
		go func() {
			out, err := a.ixFaceLooseCount(base)
			lb.mu.Lock()
			if err == nil {
				lb.last, lb.at = out, time.Now()
			}
			lb.run = nil
			lb.mu.Unlock()
			close(ch)
		}()
	}
	ch := lb.run
	lb.mu.Unlock()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.last
}

// ixFaceLooseCount works the numbers out from the faces in memory (copied,
// so grouping is not held up meanwhile).
func (a *App) ixFaceLooseCount(ctx context.Context) (*ixFaceLoose, error) {
	s := &a.ixMLBox().faces
	if err := s.load(ctx, a.db.Reader, ixFaceModel); err != nil {
		return nil, err
	}
	unit := a.ixFaceUnitOf()
	s.mu.RLock()
	n, d := len(s.ids), s.dim
	vecs := append([]int8(nil), s.vecs...)
	norms := append([]float32(nil), s.norms...)
	person := append([]int64(nil), s.person...)
	tier := append([]int8(nil), s.tier...)
	score := append([]float32(nil), s.score...)
	px := append([]float32(nil), s.px...)
	blur := append([]float32(nil), s.blur...)
	shot := make([]int64, n)
	units := make([]string, n)
	for i := range shot {
		shot[i] = s.shotLocked(int32(i))
		units[i] = unit(s.keys[s.key[i]])
	}
	g := s.gates
	s.mu.RUnlock()

	out := &ixFaceLoose{Gates: g}
	var sc, pxs, bl []float64
	var goodTodo, weakTodo []int32 // loose usable faces
	var grouped []int32            // usable faces with a person (weak faces are looked at against these only)
	var loose []int32              // usable faces without
	for i := 0; i < n; i++ {
		if norms[i] == 0 {
			continue
		}
		out.Faces++
		sc = append(sc, float64(score[i]))
		if px[i] > 0 {
			pxs = append(pxs, float64(px[i]))
		}
		if blur[i] >= 0 {
			bl = append(bl, float64(blur[i]))
		}
		if person[i] > 0 && tier[i] != ixFaceJunk {
			grouped = append(grouped, int32(i))
		}
		if person[i] != 0 {
			continue
		}
		if tier[i] != ixFaceJunk {
			loose = append(loose, int32(i))
		}
		out.Loose++
		switch {
		case px[i] > 0 && float64(px[i]) < ixFaceJunkPx:
			out.Small++
		case tier[i] == ixFaceJunk:
			out.Blurry++
		case tier[i] == ixFaceWeak:
			out.Weak++
			if float64(score[i]) < g.Score {
				out.WeakScore++
			}
			if px[i] > 0 && float64(px[i]) < g.Px {
				out.WeakSize++
			}
			if blur[i] >= 0 && float64(blur[i]) < g.Blur {
				out.WeakSharp++
			}
			weakTodo = append(weakTodo, int32(i))
		default:
			out.Good++
			goodTodo = append(goodTodo, int32(i))
		}
	}
	for _, x := range []struct {
		v    []float64
		out  *[3]float64
		unit float64
	}{{sc, &out.Score, 100}, {pxs, &out.Px, 10}, {bl, &out.Blur, 10}} {
		sort.Float64s(x.v)
		for k, p := range []float64{0.1, 0.5, 0.9} {
			x.out[k] = math.Round(ixFacePct(x.v, p)*x.unit) / x.unit
		}
	}

	// Each loose good face against all others, each loose weak one against
	// the grouped faces; a few at a time.
	const (
		kAlone = iota
		kFew
		kReady
		kWeakNear
		kWeakOther
	)
	// In a large archive, an even spread of ixFaceLooseSample good and as
	// many weak faces stand for all (the counts are scaled up).
	sample := func(l []int32) []int32 {
		if len(l) <= ixFaceLooseSample {
			return l
		}
		out := make([]int32, ixFaceLooseSample)
		for k := range out {
			out[k] = l[k*len(l)/ixFaceLooseSample]
		}
		return out
	}
	goodLook, weakLook := sample(goodTodo), sample(weakTodo)
	if len(goodLook) < len(goodTodo) || len(weakLook) < len(weakTodo) {
		out.Sampled = len(goodLook) + len(weakLook)
	}
	todo := append(append([]int32(nil), goodLook...), weakLook...)
	kind := make([]int8, len(todo))
	var next int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < max(1, runtime.NumCPU()/2); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				mu.Lock()
				t := next
				next++
				mu.Unlock()
				if int(t) >= len(todo) {
					return
				}
				i := todo[t]
				v := vecs[int(i)*d : int(i+1)*d]
				near, nearPerson, weakBand := false, false, false
				seen := map[int64]bool{shot[i]: true}
				files, good := map[string]bool{units[i]: true}, map[string]bool{units[i]: true}
				look := func(j int) {
					if j == int(i) || norms[j] == 0 || tier[j] == ixFaceJunk {
						return
					}
					c := float32(ixFaceDot8(v, vecs[j*d:(j+1)*d])) / (norms[i] * norms[j])
					if c < ixFaceSameSim {
						return
					}
					if c < ixFacePairSim(tier[i], tier[j]) {
						if person[j] > 0 {
							weakBand = true
						}
						return
					}
					near = true
					switch {
					case person[j] > 0:
						nearPerson = true
					case person[j] == 0 && !seen[shot[j]]:
						seen[shot[j]] = true
						files[units[j]] = true
						if tier[j] == ixFaceGood {
							good[units[j]] = true
						}
					}
				}
				// Grouped faces first; a good face stops looking once it is
				// known that it could be grouped now.
				for _, j := range grouped {
					if look(int(j)); nearPerson && tier[i] == ixFaceGood {
						break
					}
				}
				if tier[i] == ixFaceGood && !nearPerson {
					for _, j := range loose {
						if look(int(j)); len(files) >= ixFaceMinFaces && len(good) >= ixFaceMinGood {
							break
						}
					}
				}
				var k int8
				switch {
				case tier[i] == ixFaceWeak && !nearPerson && weakBand:
					k = kWeakNear
				case tier[i] == ixFaceWeak:
					k = kWeakOther
				case nearPerson || len(files) >= ixFaceMinFaces && len(good) >= ixFaceMinGood:
					k = kReady
				case near:
					k = kFew
				default:
					k = kAlone
				}
				kind[t] = k
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var counts [5]int
	for _, k := range kind {
		counts[k]++
	}
	scale := func(n, of, all int) int {
		if of == 0 {
			return 0
		}
		return int(math.Round(float64(n) * float64(all) / float64(of)))
	}
	out.Alone = scale(counts[kAlone], len(goodLook), len(goodTodo))
	out.Few = scale(counts[kFew], len(goodLook), len(goodTodo))
	out.Ready = scale(counts[kReady], len(goodLook), len(goodTodo))
	out.WeakNear = scale(counts[kWeakNear], len(weakLook), len(weakTodo))
	return out, nil
}

// ixFaceDot8 is ixDot8 in eight sums at a time (about twice as fast).
func ixFaceDot8(a, b []int8) int32 {
	var s0, s1, s2, s3, s4, s5, s6, s7 int32
	n := len(a) &^ 7
	b = b[:len(a)]
	for i := 0; i < n; i += 8 {
		s0 += int32(a[i]) * int32(b[i])
		s1 += int32(a[i+1]) * int32(b[i+1])
		s2 += int32(a[i+2]) * int32(b[i+2])
		s3 += int32(a[i+3]) * int32(b[i+3])
		s4 += int32(a[i+4]) * int32(b[i+4])
		s5 += int32(a[i+5]) * int32(b[i+5])
		s6 += int32(a[i+6]) * int32(b[i+6])
		s7 += int32(a[i+7]) * int32(b[i+7])
	}
	for i := n; i < len(a); i++ {
		s0 += int32(a[i]) * int32(b[i])
	}
	return s0 + s1 + s2 + s3 + s4 + s5 + s6 + s7
}

// ixFaceLooseLines is the breakdown as two lines of the diagnostics report.
func ixFaceLooseLines(l *ixFaceLoose) []string {
	if l == nil {
		return []string{"  not grouped: not worked out (try again in a minute)"}
	}
	r := func(v [3]float64, f string) string {
		return fmt.Sprintf(f+"/"+f+"/"+f, v[0], v[1], v[2])
	}
	est := ""
	if l.Sampled > 0 {
		est = fmt.Sprintf(" (look-alikes estimated from %d faces)", l.Sampled)
	}
	return []string{
		fmt.Sprintf("  not grouped %d: unusable %d (small %d, blurry %d) · weak %d (score %d, size %d, sharpness %d; %d only 0.5-0.6 alike to a person) · good %d (alone %d, look-alikes in <3 files %d, could group now %d)%s",
			l.Loose, l.Small+l.Blurry, l.Small, l.Blurry, l.Weak, l.WeakScore, l.WeakSize, l.WeakSharp, l.WeakNear, l.Good, l.Alone, l.Few, l.Ready, est),
		fmt.Sprintf("  all %d faces p10/p50/p90: score %s · box %s px · sharpness %s · start gates now: score %.2f, box %.0f px, sharpness %.0f",
			l.Faces, r(l.Score, "%.2f"), r(l.Px, "%.0f"), r(l.Blur, "%.0f"), l.Gates.Score, l.Gates.Px, l.Gates.Blur),
	}
}

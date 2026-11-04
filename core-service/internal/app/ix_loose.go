package app

import "sort"

// The likeness below which a one-off file of a set is "Probably not part
// of it" depends on the image model: one model's cosines are not
// another's (a larger model, say, gives different numbers for the same
// pictures). So the bar is measured on the archive itself. Core files (in
// two or more posts of a set) surely belong; for each, its closest other
// core file in the set is found, and a one-off file is loose when it is
// less like the core than 95% of core files are (the 5th percentile).
//
// It is sampled (up to 16 core files from each of up to 400 sets, spread
// over all sets), costs well under a second, and is kept within 0.35-0.85.
// With too little to measure (under 10 sets or 200 values) the fixed 0.62
// stays.

const (
	ixLooseSets     = 400
	ixLoosePerSet   = 16
	ixLooseMinSets  = 10
	ixLooseMinVals  = 200
	ixLooseQuantile = 0.05
	ixLooseLow      = 0.35
	ixLooseHigh     = 0.85
)

// ixLooseThreshold measures the bar from each set's core files (see the top
// of the file). emb compares two files by their embeddings alone.
func ixLooseThreshold(emb func(a, b string) (float32, bool), cores [][]string) float32 {
	if emb == nil {
		return ixSetLooseSim
	}
	step := max(1, len(cores)/ixLooseSets)
	var vals []float32
	sets := 0
	for si := 0; si < len(cores) && sets < ixLooseSets; si += step {
		keys := cores[si]
		if len(keys) < 3 {
			continue
		}
		if len(keys) > ixLoosePerSet {
			keys = keys[:ixLoosePerSet]
		}
		got := false
		for i, a := range keys {
			best, ok := float32(-2), false
			for j, b := range keys {
				if i == j {
					continue
				}
				if v, has := emb(a, b); has && v > best {
					best, ok = v, true
				}
			}
			if ok {
				vals = append(vals, best)
				got = true
			}
		}
		if got {
			sets++
		}
	}
	if sets < ixLooseMinSets || len(vals) < ixLooseMinVals {
		return ixSetLooseSim
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	q := vals[int(ixLooseQuantile*float64(len(vals)))]
	return min(ixLooseHigh, max(ixLooseLow, q))
}

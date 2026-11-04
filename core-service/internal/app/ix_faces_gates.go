package app

import (
	"math"
	"sort"
)

// Gates that follow the archive. The fixed numbers of ix_faces_quality.go
// (score 0.8, 80 px, sharpness 50) suit camera photos. Self-filmed content
// (webcam, front camera, low light, filters) has few faces that clear all
// three, so most faces stayed "weak": they could never start a person, and
// could join one only at the stricter likeness, so one person after another
// was never made. So once enough faces are measured, a face may start a
// person when it is among the better 80% of this archive's own faces:
//   - sharpness: the 20th percentile of the archive's faces, never below 15
//     (well above Ente's hard limit 10) and never above the usual 50;
//   - box: the 20th percentile, never below 64 px (a little over half the
//     112 px the model reads faces at) and never above the usual 80;
//   - detection score: the 20th percentile, never below 0.75 (Ente's
//     "minimum to show") and never above the usual 0.8.
//
// An archive of clear faces keeps the usual numbers. What protects against
// wrong people does not move: weak faces still only join, and only at the
// stricter likeness; unusable faces (under 40 px, sharpness under 10) are
// never grouped; a person still needs look-alikes in three files; two faces
// of one picture are never one person.
var ixFaceGateMin = 200 // measured faces needed before a gate follows the archive

const (
	ixFaceGatePct       = 0.20 // share of faces allowed under a gate
	ixFaceGateBlurFloor = 15.0
	ixFaceGatePxFloor   = 64.0
	ixFaceGateScoreLow  = 0.75
)

// ixFaceGates are what a face needs to start a person (and to join at the
// usual likeness).
type ixFaceGates struct {
	Score float64 `json:"score"`
	Px    float64 `json:"px"`
	Blur  float64 `json:"sharpness"`
}

var ixFaceGatesFixed = ixFaceGates{Score: ixFaceCoreScore, Px: ixFaceCorePx, Blur: ixFaceCoreBlur}

// tierOf says what kind a face is under these gates (see ixFaceTierOf).
func (g ixFaceGates) tierOf(score, px, blur float64) int8 {
	if px > 0 && px < ixFaceJunkPx || blur >= 0 && blur < ixFaceJunkBlur {
		return ixFaceJunk
	}
	if score >= g.Score && (px <= 0 || px >= g.Px) && (blur < 0 || blur >= g.Blur) {
		return ixFaceGood
	}
	return ixFaceWeak
}

// ixFacePct of sorted values (nearest rank, p in 0..1).
func ixFacePct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(len(sorted)-1, i))]
}

// ixFaceGatesFrom works the gates out from the faces' scores, sizes and
// sharpness (unknown sizes and sharpness, 0 and below zero, are left out).
func ixFaceGatesFrom(score, px, blur []float32) ixFaceGates {
	g := ixFaceGatesFixed
	pick := func(vals []float32, keep func(float64) bool) []float64 {
		var out []float64
		for _, v := range vals {
			if x := float64(v); keep(x) {
				out = append(out, x)
			}
		}
		sort.Float64s(out)
		return out
	}
	if s := pick(score, func(x float64) bool { return x > 0 }); len(s) >= ixFaceGateMin {
		g.Score = math.Max(ixFaceGateScoreLow, math.Min(ixFaceCoreScore, math.Floor(ixFacePct(s, ixFaceGatePct)*100)/100))
	}
	if s := pick(px, func(x float64) bool { return x > 0 }); len(s) >= ixFaceGateMin {
		g.Px = math.Max(ixFaceGatePxFloor, math.Min(ixFaceCorePx, math.Floor(ixFacePct(s, ixFaceGatePct))))
	}
	if s := pick(blur, func(x float64) bool { return x >= 0 }); len(s) >= ixFaceGateMin {
		g.Blur = math.Max(ixFaceGateBlurFloor, math.Min(ixFaceCoreBlur, math.Floor(ixFacePct(s, ixFaceGatePct))))
	}
	return g
}

// regateSoonLocked works the gates out again when the faces grew by a
// tenth since the last time (store write lock held), and puts every face
// in its kind under them when they moved.
func (s *ixFaceStore) regateSoonLocked() {
	if n := len(s.ids); n < ixFaceGateMin || s.gatesAt > 0 && n < s.gatesAt+s.gatesAt/10 {
		return
	}
	s.regateLocked()
}

func (s *ixFaceStore) regateLocked() {
	s.gatesAt = len(s.ids)
	sc := make([]float32, 0, len(s.ids))
	px := make([]float32, 0, len(s.ids))
	bl := make([]float32, 0, len(s.ids))
	for i := range s.ids {
		if s.norms[i] != 0 {
			sc, px, bl = append(sc, s.score[i]), append(px, s.px[i]), append(bl, s.blur[i])
		}
	}
	g := ixFaceGatesFrom(sc, px, bl)
	if g == s.gates {
		return
	}
	s.gates = g
	for i := range s.ids {
		if s.norms[i] != 0 {
			s.tier[i] = g.tierOf(float64(s.score[i]), float64(s.px[i]), float64(s.blur[i]))
		}
	}
}

// gatesNow says the gates in use.
func (s *ixFaceStore) gatesNow() ixFaceGates {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gates
}


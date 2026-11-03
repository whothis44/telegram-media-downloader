package app

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/bits"
	"sort"
)

// PDQ, the perceptual picture hash Meta published for finding copies of the
// same picture (ThreatExchange): 256 bits from the low frequencies of the
// picture's brightness, plus a quality score. Pictures that are the same
// after recompression or resizing are a few bits apart; unrelated ones about
// 128. The published guidance: up to 31 bits apart is a match, and hashes
// with quality under 50 (flat, nearly blank pictures) are not trusted.
//
// This follows the reference implementation: luma, two passes of Jarosz
// (box) filters, decimation to 64×64, a 16×16 DCT without the DC terms,
// each bit set when its coefficient is above the median.

type ixPDQ [4]uint64

func (h ixPDQ) Bytes() []byte {
	out := make([]byte, 32)
	for i, w := range h {
		for b := 0; b < 8; b++ {
			out[i*8+b] = byte(w >> (8 * b))
		}
	}
	return out
}

func ixPDQFromBytes(b []byte) (ixPDQ, bool) {
	var h ixPDQ
	if len(b) != 32 {
		return h, false
	}
	for i := range h {
		for k := 0; k < 8; k++ {
			h[i] |= uint64(b[i*8+k]) << (8 * k)
		}
	}
	return h, true
}

func (h ixPDQ) Distance(o ixPDQ) int {
	return bits.OnesCount64(h[0]^o[0]) + bits.OnesCount64(h[1]^o[1]) + bits.OnesCount64(h[2]^o[2]) + bits.OnesCount64(h[3]^o[3])
}

var ixPDQDCT = func() [16][64]float64 {
	var d [16][64]float64
	scale := math.Sqrt(2.0 / 64.0)
	for i := 0; i < 16; i++ {
		for j := 0; j < 64; j++ {
			d[i][j] = scale * math.Cos((math.Pi/2/64.0)*float64(i+1)*float64(2*j+1))
		}
	}
	return d
}()

func ixJaroszWindow(oldDim, newDim int) int {
	return (oldDim + 2*newDim - 1) / (2 * newDim)
}

func ixBox1D(in, out []float64, length, stride, window int) {
	half := (window + 2) / 2
	p1, p2, p3, p4 := half-1, window-half+1, length-window, half-1
	li, ri, oi := 0, 0, 0
	sum, n := 0.0, 0
	for i := 0; i < p1 && ri < length*stride; i++ {
		sum += in[ri]
		n++
		ri += stride
	}
	for i := 0; i < p2 && ri < length*stride; i++ {
		sum += in[ri]
		n++
		out[oi] = sum / float64(n)
		ri += stride
		oi += stride
	}
	for i := 0; i < p3; i++ {
		sum += in[ri]
		sum -= in[li]
		out[oi] = sum / float64(n)
		li += stride
		ri += stride
		oi += stride
	}
	for i := 0; i < p4 && oi < length*stride; i++ {
		sum -= in[li]
		n--
		if n > 0 {
			out[oi] = sum / float64(n)
		}
		li += stride
		oi += stride
	}
}

// ixPDQHash hashes a rows×cols luma buffer (0–255). It returns the hash, the
// quality (0–100) and the 64×64 decimated picture.
func ixPDQHash(luma []float64, rows, cols int) (ixPDQ, int, [64][64]float64) {
	var buf64 [64][64]float64
	var h ixPDQ
	if rows <= 0 || cols <= 0 || len(luma) < rows*cols {
		return h, 0, buf64
	}
	a := append([]float64(nil), luma[:rows*cols]...)
	b := make([]float64, rows*cols)
	wr := ixJaroszWindow(cols, 64)
	wc := ixJaroszWindow(rows, 64)
	for pass := 0; pass < 2; pass++ {
		if wr > 1 {
			for r := 0; r < rows; r++ {
				ixBox1D(a[r*cols:], b[r*cols:], cols, 1, wr)
			}
		} else {
			copy(b, a)
		}
		if wc > 1 {
			for c := 0; c < cols; c++ {
				ixBox1D(b[c:], a[c:], rows, cols, wc)
			}
		} else {
			copy(a, b)
		}
	}
	for i := 0; i < 64; i++ {
		ini := int((float64(i) + 0.5) * float64(rows) / 64)
		for j := 0; j < 64; j++ {
			inj := int((float64(j) + 0.5) * float64(cols) / 64)
			buf64[i][j] = a[ini*cols+inj]
		}
	}
	grad := 0
	for i := 0; i < 63; i++ {
		for j := 0; j < 64; j++ {
			grad += int(math.Abs((buf64[i][j] - buf64[i+1][j]) * 100 / 255))
		}
	}
	for i := 0; i < 64; i++ {
		for j := 0; j < 63; j++ {
			grad += int(math.Abs((buf64[i][j] - buf64[i][j+1]) * 100 / 255))
		}
	}
	quality := min(grad/90, 100)
	var t [16][64]float64
	for i := 0; i < 16; i++ {
		for j := 0; j < 64; j++ {
			s := 0.0
			for k := 0; k < 64; k++ {
				s += ixPDQDCT[i][k] * buf64[k][j]
			}
			t[i][j] = s
		}
	}
	var coef [256]float64
	for i := 0; i < 16; i++ {
		for j := 0; j < 16; j++ {
			s := 0.0
			for k := 0; k < 64; k++ {
				s += t[i][k] * ixPDQDCT[j][k]
			}
			coef[i*16+j] = s
		}
	}
	sorted := append([]float64(nil), coef[:]...)
	sort.Float64s(sorted)
	median := sorted[127] // torben's median of an even count is the lower middle
	for k, c := range coef {
		if c > median {
			h[k/64] |= 1 << uint(k%64)
		}
	}
	return h, quality, buf64
}

// ixLuma32 shrinks the 64×64 decimated picture to 32×32 bytes.
func ixLuma32(b [64][64]float64) []byte {
	out := make([]byte, 1024)
	for i := 0; i < 32; i++ {
		for j := 0; j < 32; j++ {
			v := (b[2*i][2*j] + b[2*i+1][2*j] + b[2*i][2*j+1] + b[2*i+1][2*j+1]) / 4
			out[i*32+j] = byte(math.Max(0, math.Min(255, math.Round(v))))
		}
	}
	return out
}

// ixSSIM compares two same-sized grey pictures (1 = identical), as one
// window over the whole picture plus the mean of 8×8 windows.
func ixSSIM(a, b []byte, side int) float64 {
	if len(a) != len(b) || len(a) != side*side || side < 8 {
		return 0
	}
	const c1, c2 = (0.01 * 255) * (0.01 * 255), (0.03 * 255) * (0.03 * 255)
	window := func(x0, y0, w int) float64 {
		var ma, mb float64
		n := float64(w * w)
		for y := y0; y < y0+w; y++ {
			for x := x0; x < x0+w; x++ {
				ma += float64(a[y*side+x])
				mb += float64(b[y*side+x])
			}
		}
		ma /= n
		mb /= n
		var va, vb, cov float64
		for y := y0; y < y0+w; y++ {
			for x := x0; x < x0+w; x++ {
				da, db := float64(a[y*side+x])-ma, float64(b[y*side+x])-mb
				va += da * da
				vb += db * db
				cov += da * db
			}
		}
		va /= n - 1
		vb /= n - 1
		cov /= n - 1
		return ((2*ma*mb + c1) * (2*cov + c2)) / ((ma*ma + mb*mb + c1) * (va + vb + c2))
	}
	sum, count := 0.0, 0
	for y := 0; y+8 <= side; y += 4 {
		for x := 0; x+8 <= side; x += 4 {
			sum += window(x, y, 8)
			count++
		}
	}
	return sum / float64(count)
}

// ---------- reading pictures ----------

// ixPicture is what one read of a picture gives: its luma (shrunk to at
// most 512 on the long side, for hashing), and a small JPEG for the grid.
type ixPicture struct {
	Width, Height int
	Luma          []float64
	Rows, Cols    int
	Thumb         []byte
}

const ixThumbSide = 400 // the longer side of grid thumbnails

// ixReadPicture decodes a picture once and derives everything from it.
func ixReadPicture(data []byte) (*ixPicture, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return nil, image.ErrFormat
	}
	p := &ixPicture{Width: w, Height: h}
	// Luma, averaged down by a whole factor to at most 512 on the long side.
	f := max(1, (max(w, h)+511)/512)
	p.Cols, p.Rows = max(1, w/f), max(1, h/f)
	p.Luma = make([]float64, p.Rows*p.Cols)
	ycc, isYCC := img.(*image.YCbCr)
	gray, isGray := img.(*image.Gray)
	for r := 0; r < p.Rows; r++ {
		for c := 0; c < p.Cols; c++ {
			sum := 0.0
			for dy := 0; dy < f; dy++ {
				y := bounds.Min.Y + r*f + dy
				for dx := 0; dx < f; dx++ {
					x := bounds.Min.X + c*f + dx
					switch {
					case isYCC:
						sum += float64(ycc.Y[ycc.YOffset(x, y)])
					case isGray:
						sum += float64(gray.Pix[gray.PixOffset(x, y)])
					default:
						cr, cg, cb, _ := img.At(x, y).RGBA()
						sum += (0.299*float64(cr) + 0.587*float64(cg) + 0.114*float64(cb)) / 257
					}
				}
			}
			p.Luma[r*p.Cols+c] = sum / float64(f*f)
		}
	}
	thumb, err := ixMakeThumb(img, ixThumbSide)
	if err == nil {
		p.Thumb = thumb
	}
	return p, nil
}

// ixMakeThumb shrinks a picture to fit side×side (area averaging) as JPEG.
func ixMakeThumb(img image.Image, side int) ([]byte, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := math.Min(1, float64(side)/float64(max(w, h)))
	tw, th := max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale)))
	out := image.NewRGBA(image.Rect(0, 0, tw, th))
	ycc, isYCC := img.(*image.YCbCr)
	for ty := 0; ty < th; ty++ {
		y0 := b.Min.Y + ty*h/th
		y1 := max(y0+1, b.Min.Y+(ty+1)*h/th)
		for tx := 0; tx < tw; tx++ {
			x0 := b.Min.X + tx*w/tw
			x1 := max(x0+1, b.Min.X+(tx+1)*w/tw)
			var sr, sg, sb float64
			n := 0
			// Large blocks are sampled on a grid of at most 4×4 points.
			stepY, stepX := max(1, (y1-y0)/4), max(1, (x1-x0)/4)
			for y := y0; y < y1; y += stepY {
				for x := x0; x < x1; x += stepX {
					var r8, g8, b8 uint8
					if isYCC {
						yi, ci := ycc.YOffset(x, y), ycc.COffset(x, y)
						r8, g8, b8 = color.YCbCrToRGB(ycc.Y[yi], ycc.Cb[ci], ycc.Cr[ci])
					} else {
						cr, cg, cb, _ := img.At(x, y).RGBA()
						r8, g8, b8 = uint8(cr>>8), uint8(cg>>8), uint8(cb>>8)
					}
					sr += float64(r8)
					sg += float64(g8)
					sb += float64(b8)
					n++
				}
			}
			out.SetRGBA(tx, ty, color.RGBA{uint8(sr / float64(n)), uint8(sg / float64(n)), uint8(sb / float64(n)), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

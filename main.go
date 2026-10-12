//go:build js && wasm

// Kociemba two-phase solver compiled to WebAssembly.
//
// Move semantics are derived geometrically, exactly like src/lib/cube/core.ts,
// so a move here means the same thing as a move there. Coordinates and pruning
// tables follow the classic two-phase formulation:
//
//   phase 1: corner orientation (2187), edge orientation (2048), U/D-edge
//            slice (495) -> reach orientation-solved + slice-solved
//   phase 2: corner permutation (40320), U/D-edge permutation (40320),
//            middle-edge permutation (24) -> reach fully solved using only
//            U D R2 L2 F2 B2
//
// Tables are built once at init and reported back to JS as progress so the
// worker can show a bar without binary search heuristics.
package main

import (
	"fmt"
	"math/bits"
	"math/rand"
	"strings"
	"syscall/js"
	"time"
)

// --------------------------------------------------------------------------
// geometry
// --------------------------------------------------------------------------

type vec3 [3]int

var faceDefs = []struct {
	face   byte
	normal vec3
	at     func(r, c int) vec3
}{
	{'U', vec3{0, 1, 0}, func(r, c int) vec3 { return vec3{c - 1, 1, r - 1} }},
	{'R', vec3{1, 0, 0}, func(r, c int) vec3 { return vec3{1, 1 - r, 1 - c} }},
	{'F', vec3{0, 0, 1}, func(r, c int) vec3 { return vec3{c - 1, 1 - r, 1} }},
	{'D', vec3{0, -1, 0}, func(r, c int) vec3 { return vec3{c - 1, -1, 1 - r} }},
	{'L', vec3{-1, 0, 0}, func(r, c int) vec3 { return vec3{-1, 1 - r, c - 1} }},
	{'B', vec3{0, 0, -1}, func(r, c int) vec3 { return vec3{1 - c, 1 - r, -1} }},
}

var (
	faceletPos  [54]vec3
	faceletNorm [54]vec3
	faceletByKey [729]int
)

func posIdx(p vec3) int  { return ((p[0]+1)*3+(p[1]+1))*3 + (p[2] + 1) }
func normIdx(n vec3) int { return ((n[0]+1)*3+(n[1]+1))*3 + (n[2] + 1) }

func initGeometry() {
	for i := range faceletByKey {
		faceletByKey[i] = -1
	}
	idx := 0
	for _, def := range faceDefs {
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				pos := def.at(r, c)
				faceletPos[idx] = pos
				faceletNorm[idx] = def.normal
				faceletByKey[posIdx(pos)*27+normIdx(def.normal)] = idx
				idx++
			}
		}
	}
}

func faceletIndex(p, n vec3) int {
	return faceletByKey[posIdx(p)*27+normIdx(n)]
}

func rot90(v vec3, axis int) vec3 {
	switch axis {
	case 0:
		return vec3{v[0], -v[2], v[1]}
	case 1:
		return vec3{v[2], v[1], -v[0]}
	default:
		return vec3{-v[1], v[0], v[2]}
	}
}

func rotateVec(v vec3, axis, dir int) vec3 {
	times := 1
	if dir < 0 {
		times = 3
	}
	for i := 0; i < times; i++ {
		v = rot90(v, axis)
	}
	return v
}

type moveDef struct {
	name  string
	axis  int
	dir   int
	layer func(x, y, z int) bool
}

var faceMoveDefs = []moveDef{
	{"U", 1, -1, func(x, y, z int) bool { return y == 1 }},
	{"R", 0, -1, func(x, y, z int) bool { return x == 1 }},
	{"F", 2, -1, func(x, y, z int) bool { return z == 1 }},
	{"D", 1, 1, func(x, y, z int) bool { return y == -1 }},
	{"L", 0, 1, func(x, y, z int) bool { return x == -1 }},
	{"B", 2, 1, func(x, y, z int) bool { return z == -1 }},
}

func buildFaceletPerm(def moveDef) [54]int {
	var perm [54]int
	for i := 0; i < 54; i++ {
		p := faceletPos[i]
		if !def.layer(p[0], p[1], p[2]) {
			perm[i] = i
			continue
		}
		np := rotateVec(p, def.axis, def.dir)
		nn := rotateVec(faceletNorm[i], def.axis, def.dir)
		perm[faceletIndex(np, nn)] = i
	}
	return perm
}

func applyPerm(state string, perm [54]int) string {
	var b [54]byte
	for i := 0; i < 54; i++ {
		b[i] = state[perm[i]]
	}
	return string(b[:])
}

func invertPerm(perm [54]int) [54]int {
	var out [54]int
	for i := 0; i < 54; i++ {
		out[perm[i]] = i
	}
	return out
}

func composePerm(a, b [54]int) [54]int {
	var out [54]int
	for i := 0; i < 54; i++ {
		out[i] = a[b[i]]
	}
	return out
}

// --------------------------------------------------------------------------
// move list: U U2 U' R R2 R' F F2 F' D D2 D' L L2 L' B B2 B'
// --------------------------------------------------------------------------

var (
	mvToken   [18]string
	faceletPm [18][54]int
	mvFace    [18]int
	mvAxis    [18]int
	phase2Mvs = []int{0, 1, 2, 9, 10, 11, 4, 7, 13, 16}
)

func initMoves() {
	names := []string{"U", "R", "F", "D", "L", "B"}
	axisOf := map[string]int{"U": 1, "R": 0, "F": 2, "D": 1, "L": 0, "B": 2}
	faceOf := map[string]int{"U": 0, "R": 1, "F": 2, "D": 3, "L": 4, "B": 5}

	m := 0
	for i, def := range faceMoveDefs {
		base := buildFaceletPerm(def)
		dbl := composePerm(base, base)
		inv := invertPerm(base)
		variants := []struct {
			token string
			perm  [54]int
		}{
			{names[i], base},
			{names[i] + "2", dbl},
			{names[i] + "'", inv},
		}
		for _, v := range variants {
			mvToken[m] = v.token
			faceletPm[m] = v.perm
			faceOfName := names[i]
			mvFace[m] = faceOf[faceOfName]
			mvAxis[m] = axisOf[faceOfName]
			m++
		}
	}
}

const solvedFacelets = "UUUUUUUUURRRRRRRRRFFFFFFFFFDDDDDDDDDLLLLLLLLLBBBBBBBBB"

// --------------------------------------------------------------------------
// cubie model
// --------------------------------------------------------------------------

type cubie struct {
	cp [8]int8
	co [8]int8
	ep [12]int8
	eo [12]int8
}

var cornerColors = [8]string{"URF", "UFL", "ULB", "UBR", "DFR", "DLF", "DBL", "DRB"}
var edgeColors = [12]string{"UR", "UF", "UL", "UB", "DR", "DF", "DL", "DB", "FR", "FL", "BL", "BR"}

var cornerFacelets = [8][3]int{
	{8, 9, 20}, {6, 18, 38}, {0, 36, 47}, {2, 45, 11},
	{29, 26, 15}, {27, 44, 24}, {33, 53, 42}, {35, 17, 51},
}

var edgeFacelets = [12][2]int{
	{5, 10}, {7, 19}, {3, 37}, {1, 46},
	{32, 16}, {28, 25}, {30, 43}, {34, 52},
	{23, 12}, {21, 41}, {50, 39}, {48, 14},
}

func sortedKey(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		for j := i + 1; j < len(b); j++ {
			if b[j] < b[i] {
				b[i], b[j] = b[j], b[i]
			}
		}
	}
	return string(b)
}

var cornerByColors = map[string]int{}
var edgeByColors = map[string]int{}

func initCubieTables() {
	for i, c := range cornerColors {
		cornerByColors[sortedKey(c)] = i
	}
	for i, c := range edgeColors {
		edgeByColors[sortedKey(c)] = i
	}
}

func solvedCubie() cubie {
	var c cubie
	for i := 0; i < 8; i++ {
		c.cp[i] = int8(i)
	}
	for i := 0; i < 12; i++ {
		c.ep[i] = int8(i)
	}
	return c
}

func parseFacelets(state string) (cubie, string) {
	var c cubie
	if len(state) != 54 {
		return c, "State must be 54 stickers."
	}
	var counts [91]int
	for i := 0; i < 54; i++ {
		counts[state[i]]++
	}
	for _, f := range []byte("URFDLB") {
		if counts[f] != 9 {
			return c, "A color is missing or duplicated."
		}
	}
	centerFaces := []byte("URFDLB")
	centerIdx := [6]int{4, 13, 22, 31, 40, 49}
	for k, i := range centerIdx {
		if state[i] != centerFaces[k] {
			return c, "Centre pieces moved."
		}
	}

	var seenC [8]bool
	var seenESet [12]bool

	for i := 0; i < 8; i++ {
		fl := cornerFacelets[i]
		key := sortedKey(string([]byte{state[fl[0]], state[fl[1]], state[fl[2]]}))
		piece, ok := cornerByColors[key]
		if !ok {
			return c, "That corner does not exist."
		}
		if seenC[piece] {
			return c, "Two corners are the same."
		}
		seenC[piece] = true
		ori := -1
		for j := 0; j < 3; j++ {
			ch := state[fl[j]]
			if ch == 'U' || ch == 'D' {
				ori = j
				break
			}
		}
		if ori < 0 {
			return c, "A corner has no white or yellow sticker."
		}
		c.cp[i] = int8(piece)
		c.co[i] = int8(ori)
	}

	for i := 0; i < 12; i++ {
		fl := edgeFacelets[i]
		key := sortedKey(string([]byte{state[fl[0]], state[fl[1]]}))
		piece, ok := edgeByColors[key]
		if !ok {
			return c, "That edge does not exist."
		}
		if seenESet[piece] {
			return c, "Two edges are the same."
		}
		seenESet[piece] = true
		// Edge orientation: the piece is oriented when its reference colour —
		// the U/D colour for U/D edges, the F/B colour for middle edges, which
		// is always colour 0 of the canonical name — sits on the slot's first
		// facelet. This is the convention under which orientation is additive
		// across moves (R/L/U/D preserve it, F/B flip their four edges), which
		// is what lets the cubie move tables be built from deltas at all.
		ori := 0
		if state[fl[0]] != edgeColors[piece][0] {
			ori = 1
		}
		c.ep[i] = int8(piece)
		c.eo[i] = int8(ori)
	}

	// orientation / parity invariants
	coSum := 0
	for _, o := range c.co {
		coSum += int(o)
	}
	if coSum%3 != 0 {
		return c, "Corner twists do not add up."
	}
	eoSum := 0
	for _, o := range c.eo {
		eoSum += int(o)
	}
	if eoSum%2 != 0 {
		return c, "Edge parity invalid."
	}
	if permParity(c.cp[:]) != permParity(c.ep[:]) {
		return c, "Two pieces are swapped."
	}
	return c, ""
}

func permParity(p []int8) int {
	seen := make([]bool, len(p))
	parity := 0
	for i := range p {
		if seen[i] {
			continue
		}
		j := i
		length := 0
		for !seen[j] {
			seen[j] = true
			j = int(p[j])
			length++
		}
		if length%2 == 0 {
			parity ^= 1
		}
	}
	return parity
}

func serializeCubie(c cubie) string {
	out := []byte("??????????????????????????????????????????????????????")
	out[4], out[13], out[22] = 'U', 'R', 'F'
	out[31], out[40], out[49] = 'D', 'L', 'B'
	for i := 0; i < 8; i++ {
		piece := cornerColors[c.cp[i]]
		ori := int(c.co[i])
		fl := cornerFacelets[i]
		for j := 0; j < 3; j++ {
			out[fl[j]] = piece[(j-ori+3)%3]
		}
	}
	for i := 0; i < 12; i++ {
		piece := edgeColors[c.ep[i]]
		fl := edgeFacelets[i]
		if c.eo[i] == 1 {
			out[fl[0]], out[fl[1]] = piece[1], piece[0]
		} else {
			out[fl[0]], out[fl[1]] = piece[0], piece[1]
		}
	}
	return string(out)
}

// --------------------------------------------------------------------------
// move application at cubie level, derived from the geometric facelet moves
// --------------------------------------------------------------------------

var (
	cornerDest  [18][8]int8
	cornerDelta [18][8]int8
	edgeDest    [18][12]int8
	edgeDelta   [18][12]int8
)

func initCubieMoves() {
	for m := 0; m < 18; m++ {
		after := applyPerm(solvedFacelets, faceletPm[m])
		parsed, err := parseFacelets(after)
		if err != "" {
			panic("solved state after move did not parse: " + err)
		}
		for i := 0; i < 8; i++ {
			for j := 0; j < 8; j++ {
				if int(parsed.cp[j]) == i {
					cornerDest[m][i] = int8(j)
					cornerDelta[m][i] = parsed.co[j]
					break
				}
			}
		}
		for i := 0; i < 12; i++ {
			for j := 0; j < 12; j++ {
				if int(parsed.ep[j]) == i {
					edgeDest[m][i] = int8(j)
					edgeDelta[m][i] = parsed.eo[j]
					break
				}
			}
		}
	}
}

func applyMove(c cubie, m int) cubie {
	var out cubie
	for i := 0; i < 8; i++ {
		d := cornerDest[m][i]
		out.cp[d] = c.cp[i]
		out.co[d] = int8((int(c.co[i]) + int(cornerDelta[m][i])) % 3)
	}
	for i := 0; i < 12; i++ {
		d := edgeDest[m][i]
		out.ep[d] = c.ep[i]
		out.eo[d] = int8((int(c.eo[i]) + int(edgeDelta[m][i])) % 2)
	}
	return out
}

// --------------------------------------------------------------------------
// coordinates
// --------------------------------------------------------------------------

var pow3 = [7]int{1, 3, 9, 27, 81, 243, 729}
var pow2 = [11]int{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024}

func twistIdx(c cubie) int {
	t := 0
	for i := 0; i < 7; i++ {
		t += int(c.co[i]) * pow3[i]
	}
	return t
}

func flipIdx(c cubie) int {
	f := 0
	for i := 0; i < 11; i++ {
		f += int(c.eo[i]) * pow2[i]
	}
	return f
}

func decodeTwist(t int) cubie {
	c := solvedCubie()
	sum := 0
	for i := 0; i < 7; i++ {
		c.co[i] = int8((t / pow3[i]) % 3)
		sum += int(c.co[i])
	}
	c.co[7] = int8((3 - sum%3) % 3)
	return c
}

func decodeFlip(f int) cubie {
	c := solvedCubie()
	sum := 0
	for i := 0; i < 11; i++ {
		c.eo[i] = int8((f / pow2[i]) % 2)
		sum += int(c.eo[i])
	}
	c.eo[11] = int8(sum % 2)
	return c
}

var (
	sliceOfMask [4096]int
	maskOfSlice [495]int
	goalSlice   int
)

func initSliceTables() {
	for i := range sliceOfMask {
		sliceOfMask[i] = -1
	}
	idx := 0
	for m := 0; m < 4096; m++ {
		if bits.OnesCount(uint(m)) == 4 {
			sliceOfMask[m] = idx
			maskOfSlice[idx] = m
			idx++
		}
	}
}

func sliceIdx(c cubie) int {
	mask := 0
	for i := 0; i < 12; i++ {
		if c.ep[i] >= 8 {
			mask |= 1 << uint(i)
		}
	}
	return sliceOfMask[mask]
}

func decodeSlice(s int) cubie {
	c := solvedCubie()
	mask := maskOfSlice[s]
	middle := []int8{8, 9, 10, 11}
	ud := []int8{0, 1, 2, 3, 4, 5, 6, 7}
	mi, ui := 0, 0
	for i := 0; i < 12; i++ {
		if mask&(1<<uint(i)) != 0 {
			c.ep[i] = middle[mi]
			mi++
		} else {
			c.ep[i] = ud[ui]
			ui++
		}
	}
	return c
}

var fact = [9]int{1, 1, 2, 6, 24, 120, 720, 5040, 40320}

func permIndex(p []int8) int {
	n := len(p)
	idx := 0
	for i := 0; i < n; i++ {
		count := 0
		for j := i + 1; j < n; j++ {
			if p[j] < p[i] {
				count++
			}
		}
		idx += count * fact[n-1-i]
	}
	return idx
}

func decodePermIndex(idx, n int) []int8 {
	items := make([]int, n)
	for i := range items {
		items[i] = i
	}
	out := make([]int8, n)
	for i := 0; i < n; i++ {
		f := fact[n-1-i]
		k := idx / f
		idx %= f
		out[i] = int8(items[k])
		items = append(items[:k], items[k+1:]...)
	}
	return out
}

func cornerPermIdx(c cubie) int { return permIndex(c.cp[:]) }

func edgePermIdx(c cubie) int { return permIndex(c.ep[0:8]) }

func slicePermIdx(c cubie) int {
	var p [4]int8
	for i := 0; i < 4; i++ {
		p[i] = c.ep[8+i] - 8
	}
	return permIndex(p[:])
}

func decodeCornerPerm(idx int) cubie {
	c := solvedCubie()
	copy(c.cp[:], decodePermIndex(idx, 8))
	return c
}

func decodeEdgePerm(idx int) cubie {
	c := solvedCubie()
	copy(c.ep[0:8], decodePermIndex(idx, 8))
	for i := 8; i < 12; i++ {
		c.ep[i] = int8(i)
	}
	return c
}

func decodeSlicePerm(idx int) cubie {
	c := solvedCubie()
	p := decodePermIndex(idx, 4)
	for i := 0; i < 4; i++ {
		c.ep[8+i] = p[i] + 8
	}
	return c
}

// --------------------------------------------------------------------------
// move + pruning tables
// --------------------------------------------------------------------------

const (
	nTwist      = 2187
	nFlip       = 2048
	nSlice      = 495
	nCornerPerm = 40320
	nEdgePerm   = 40320
	nSlicePerm  = 24
)

var (
	twistMove []int16
	flipMove  []int16
	sliceMove []int16
	// corner/edge permutation indices reach 8! - 1 = 40319, so these need 32
	// bits — int16 would wrap negative and the BFS below would skip them.
	cpMove   []int32
	epMove   []int32
	spMove   []int16
	pruneTS  []int8
	pruneFS  []int8
	pruneCPS []int8
	pruneEPS []int8
)

func reportProgress(frac float64) {
	fn := js.Global().Get("__solverProgress")
	if fn.Type() == js.TypeFunction {
		fn.Invoke(frac)
	}
}

func buildCoordinateMoves() {
	twistMove = make([]int16, nTwist*18)
	for t := 0; t < nTwist; t++ {
		c := decodeTwist(t)
		for m := 0; m < 18; m++ {
			twistMove[t*18+m] = int16(twistIdx(applyMove(c, m)))
		}
	}
	reportProgress(0.08)

	flipMove = make([]int16, nFlip*18)
	for f := 0; f < nFlip; f++ {
		c := decodeFlip(f)
		for m := 0; m < 18; m++ {
			flipMove[f*18+m] = int16(flipIdx(applyMove(c, m)))
		}
	}
	reportProgress(0.16)

	sliceMove = make([]int16, nSlice*18)
	for s := 0; s < nSlice; s++ {
		c := decodeSlice(s)
		for m := 0; m < 18; m++ {
			sliceMove[s*18+m] = int16(sliceIdx(applyMove(c, m)))
		}
	}
	reportProgress(0.24)

	cpMove = make([]int32, nCornerPerm*10)
	for p := 0; p < nCornerPerm; p++ {
		c := decodeCornerPerm(p)
		for k, m := range phase2Mvs {
			cpMove[p*10+k] = int32(cornerPermIdx(applyMove(c, m)))
		}
	}
	reportProgress(0.42)

	epMove = make([]int32, nEdgePerm*10)
	for p := 0; p < nEdgePerm; p++ {
		c := decodeEdgePerm(p)
		for k, m := range phase2Mvs {
			epMove[p*10+k] = int32(edgePermIdx(applyMove(c, m)))
		}
	}
	reportProgress(0.6)

	spMove = make([]int16, nSlicePerm*10)
	for p := 0; p < nSlicePerm; p++ {
		c := decodeSlicePerm(p)
		for k, m := range phase2Mvs {
			spMove[p*10+k] = int16(slicePermIdx(applyMove(c, m)))
		}
	}
	reportProgress(0.66)
}

type bfsQueue struct {
	data []int32
	head int
}

func newBFS(cap int) *bfsQueue {
	return &bfsQueue{data: make([]int32, 0, cap)}
}

func (q *bfsQueue) push(v int32) { q.data = append(q.data, v) }

func (q *bfsQueue) pop() (int32, bool) {
	if q.head >= len(q.data) {
		return 0, false
	}
	v := q.data[q.head]
	q.head++
	return v, true
}

func fillPrune(table []int8, startIndex int, expand func(int) [18]int32, size int) int {
	for i := range table {
		table[i] = -1
	}
	q := newBFS(size / 4)
	table[startIndex] = 0
	q.push(int32(startIndex))
	pops := 0
	for {
		cur, ok := q.pop()
		if !ok {
			break
		}
		pops++
		d := table[cur]
		neighbors := expand(int(cur))
		for _, n := range neighbors {
			if n < 0 {
				continue
			}
			if table[n] == -1 {
				table[n] = d + 1
				q.push(n)
			}
		}
	}
	return pops
}

func buildPhase1Prune() {
	pruneTS = make([]int8, nTwist*nSlice)
	fillPrune(pruneTS, 0*nSlice+goalSlice, func(idx int) [18]int32 {
		t := idx / nSlice
		s := idx % nSlice
		var out [18]int32
		for m := 0; m < 18; m++ {
			out[m] = int32(int(twistMove[t*18+m])*nSlice + int(sliceMove[s*18+m]))
		}
		return out
	}, len(pruneTS))
	reportProgress(0.78)

	pruneFS = make([]int8, nFlip*nSlice)
	fillPrune(pruneFS, 0*nSlice+goalSlice, func(idx int) [18]int32 {
		f := idx / nSlice
		s := idx % nSlice
		var out [18]int32
		for m := 0; m < 18; m++ {
			out[m] = int32(int(flipMove[f*18+m])*nSlice + int(sliceMove[s*18+m]))
		}
		return out
	}, len(pruneFS))
	reportProgress(0.88)
}

func buildPhase2Prune() {
	pruneCPS = make([]int8, nCornerPerm*nSlicePerm)
	fillPrune(pruneCPS, 0, func(idx int) [18]int32 {
		cp := idx / nSlicePerm
		sp := idx % nSlicePerm
		var out [18]int32
		for i := range out {
			out[i] = -1
		}
		for k := 0; k < 10; k++ {
			out[k] = int32(int(cpMove[cp*10+k])*nSlicePerm + int(spMove[sp*10+k]))
		}
		return out
	}, len(pruneCPS))
	reportProgress(0.94)

	pruneEPS = make([]int8, nEdgePerm*nSlicePerm)
	fillPrune(pruneEPS, 0, func(idx int) [18]int32 {
		ep := idx / nSlicePerm
		sp := idx % nSlicePerm
		var out [18]int32
		for i := range out {
			out[i] = -1
		}
		for k := 0; k < 10; k++ {
			out[k] = int32(int(epMove[ep*10+k])*nSlicePerm + int(spMove[sp*10+k]))
		}
		return out
	}, len(pruneEPS))
	reportProgress(0.99)
}

var tablesReady bool

func buildAll() {
	if tablesReady {
		return
	}
	initGeometry()
	initMoves()
	initCubieTables()
	initCubieMoves()
	initSliceTables()
	goalSlice = sliceIdx(solvedCubie())
	buildCoordinateMoves()
	buildPhase1Prune()
	buildPhase2Prune()
	tablesReady = true
	reportProgress(1)
}

// --------------------------------------------------------------------------
// search
// --------------------------------------------------------------------------

var (
	startCubie cubie
	path       [48]int
	pathLen    int
	bestPath   []int
	bestLen    int
	deadline   time.Time
	timedOut   bool
	nodeCount  int
)

func allowedMove(m, last, last2 int) bool {
	if last >= 0 && mvFace[m] == mvFace[last] {
		return false
	}
	if last >= 0 && last2 >= 0 && mvAxis[m] == mvAxis[last] && mvAxis[m] == mvAxis[last2] {
		return false
	}
	return true
}

const nodeCheckMask = 8191

func checkTimeout() {
	nodeCount++
	if nodeCount&nodeCheckMask == 0 && time.Now().After(deadline) {
		timedOut = true
	}
}

// reconstruct replays the current path onto the start state. It only runs when
// phase 1 reaches its goal, which is rare, so the search itself can stay purely
// coordinate-driven — three table lookups per node instead of re-deriving
// coordinates from a full cubie state every time.
func reconstruct(n int) cubie {
	c := startCubie
	for i := 0; i < n; i++ {
		c = applyMove(c, path[i])
	}
	return c
}

// search2 explores at most `remaining` phase-2 moves and records a candidate as
// soon as it reaches the solved coordinates. It stops at the first hit for the
// current budget: callers raise the budget from the heuristic's lower bound, so
// the first budget that yields anything already yields a short completion.
func search2(cp, ep, sp, remaining, last, last2 int) bool {
	if timedOut {
		return false
	}
	checkTimeout()

	if cp == 0 && ep == 0 && sp == 0 {
		if pathLen < bestLen {
			bestLen = pathLen
			bestPath = make([]int, pathLen)
			copy(bestPath, path[:pathLen])
		}
		return true
	}
	if remaining == 0 {
		return false
	}

	h := int(pruneCPS[cp*nSlicePerm+sp])
	if e := int(pruneEPS[ep*nSlicePerm+sp]); e > h {
		h = e
	}
	if h > remaining {
		return false
	}

	for k := 0; k < 10; k++ {
		m := phase2Mvs[k]
		if !allowedMove(m, last, last2) {
			continue
		}
		path[pathLen] = m
		pathLen++
		found := search2(int(cpMove[cp*10+k]), int(epMove[ep*10+k]), int(spMove[sp*10+k]), remaining-1, m, last)
		pathLen--
		if found {
			return true
		}
		if timedOut {
			return false
		}
	}
	return false
}

// search1 walks only the (twist, flip, slice) coordinates. Phase 1 is exact
// depth: only a state reached after exactly the requested number of moves may
// hand over, which keeps the search from wandering once its goal is met.
// Findings feed a branch-and-bound bound rather than returning immediately, so
// later goals can still improve on them.
func search1(t, f, s, remaining, last, last2 int) bool {
	if timedOut {
		return true
	}
	checkTimeout()

	if remaining == 0 {
		if t == 0 && f == 0 && s == goalSlice {
			c := reconstruct(pathLen)
			cp := cornerPermIdx(c)
			ep := edgePermIdx(c)
			sp := slicePermIdx(c)
			lower := int(pruneCPS[cp*nSlicePerm+sp])
			if e := int(pruneEPS[ep*nSlicePerm+sp]); e > lower {
				lower = e
			}
			limit := bestLen - pathLen - 1
			if limit > maxPhase2Dist {
				limit = maxPhase2Dist
			}
			for p2 := lower; p2 <= limit; p2++ {
				if search2(cp, ep, sp, p2, last, last2) {
					break
				}
				if timedOut {
					return true
				}
			}
		}
		return false
	}

	h := int(pruneTS[t*nSlice+s])
	if x := int(pruneFS[f*nSlice+s]); x > h {
		h = x
	}
	if h > remaining {
		return false
	}
	// Even a perfect finish cannot beat the incumbent.
	if pathLen+h >= bestLen {
		return false
	}

	for m := 0; m < 18; m++ {
		if !allowedMove(m, last, last2) {
			continue
		}
		path[pathLen] = m
		pathLen++
		if search1(int(twistMove[t*18+m]), int(flipMove[f*18+m]), int(sliceMove[s*18+m]), remaining-1, m, last) {
			pathLen--
			return true
		}
		pathLen--
	}
	return false
}

// maxPhase1Dist bounds phase 1: orientation plus slice is always solvable in at
// most twelve moves, so searching deeper only wastes time.
const maxPhase1Dist = 12

// maxPhase2Dist bounds the phase-2 budget handed to each phase-1 goal.
const maxPhase2Dist = 18

func solveState(c cubie, maxDepth int, timeout time.Duration) []int {
	buildAll()
	timedOut = false
	nodeCount = 0
	deadline = time.Now().Add(timeout)
	startCubie = c

	if maxDepth <= 0 || maxDepth > 30 {
		maxDepth = 22
	}

	t0 := twistIdx(c)
	f0 := flipIdx(c)
	s0 := sliceIdx(c)
	lower := int(pruneTS[t0*nSlice+s0])
	if x := int(pruneFS[f0*nSlice+s0]); x > lower {
		lower = x
	}

	bestLen = maxDepth + 1
	bestPath = nil

	for p1 := lower; p1 <= maxPhase1Dist && p1 < bestLen; p1++ {
		pathLen = 0
		search1(t0, f0, s0, p1, -1, -1)
		if timedOut {
			break
		}
	}

	if bestPath == nil {
		return nil
	}
	return bestPath
}

// --------------------------------------------------------------------------
// JS glue
// --------------------------------------------------------------------------

func jsObj() map[string]interface{} {
	return map[string]interface{}{
		"init": js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			buildAll()
			return true
		}),
		"solve": js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			res := map[string]interface{}{"solution": "", "error": ""}
			if len(args) < 1 {
				res["error"] = "Missing facelet string."
				return res
			}
			facelets := args[0].String()
			maxDepth := 22
			if len(args) > 1 && args[1].Type() == js.TypeNumber {
				maxDepth = args[1].Int()
			}
			timeoutMs := 5000
			if len(args) > 2 && args[2].Type() == js.TypeNumber {
				timeoutMs = args[2].Int()
			}
			buildAll()
			c, err := parseFacelets(facelets)
			if err != "" {
				res["error"] = err
				return res
			}
			sol := solveState(c, maxDepth, time.Duration(timeoutMs)*time.Millisecond)
			if sol == nil {
				res["error"] = "No solution within limits."
				return res
			}
			tokens := make([]string, len(sol))
			for i, m := range sol {
				tokens[i] = mvToken[m]
			}
			res["solution"] = strings.Join(tokens, " ")
			return res
		}),
		"apply": js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			if len(args) < 2 {
				return ""
			}
			state := args[0].String()
			tokens := strings.Fields(args[1].String())
			for _, t := range tokens {
				idx := -1
				for i := 0; i < 18; i++ {
					if mvToken[i] == t {
						idx = i
						break
					}
				}
				if idx < 0 {
					return ""
				}
				state = applyPerm(state, faceletPm[idx])
			}
			return state
		}),
		"isSolved": js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			if len(args) < 1 {
				return false
			}
			return args[0].String() == solvedFacelets
		}),
		"randomState": js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			seed := time.Now().UnixNano()
			if len(args) > 0 && args[0].Type() == js.TypeNumber {
				seed = int64(args[0].Int())
			}
			return buildAllRandomState(rand.New(rand.NewSource(seed)))
		}),
		"selfTest": js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			n := 25
			if len(args) > 0 && args[0].Type() == js.TypeNumber {
				n = args[0].Int()
			}
			return runSelfTest(n)
		}),
		"stats": js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			buildAll()
			count := func(t []int8) int {
				unfilled := 0
				for _, v := range t {
					if v < 0 {
						unfilled++
					}
				}
				return unfilled
			}
			return fmt.Sprintf(
				"tables ready (goalSlice=%d); unfilled twist-slice=%d flip-slice=%d corner-slice=%d edge-slice=%d",
				goalSlice, count(pruneTS), count(pruneFS), count(pruneCPS), count(pruneEPS),
			)
		}),
	}
}

func buildAllRandomState(rng *rand.Rand) string {
	c := solvedCubie()
	// random corner permutation
	cp := rng.Perm(8)
	for i := 0; i < 8; i++ {
		c.cp[i] = int8(cp[i])
	}
	ep := rng.Perm(12)
	for i := 0; i < 12; i++ {
		c.ep[i] = int8(ep[i])
	}
	sum := 0
	for i := 0; i < 7; i++ {
		c.co[i] = int8(rng.Intn(3))
		sum += int(c.co[i])
	}
	c.co[7] = int8((3 - sum%3) % 3)
	sum = 0
	for i := 0; i < 11; i++ {
		c.eo[i] = int8(rng.Intn(2))
		sum += int(c.eo[i])
	}
	c.eo[11] = int8(sum % 2)
	if permParity(c.cp[:]) != permParity(c.ep[:]) {
		c.ep[0], c.ep[1] = c.ep[1], c.ep[0]
	}
	return serializeCubie(c)
}

func runSelfTest(n int) map[string]interface{} {
	buildAll()
	rng := rand.New(rand.NewSource(20260101))
	passed, failed := 0, 0
	var worst time.Duration
	var total time.Duration
	for i := 0; i < n; i++ {
		state := buildAllRandomState(rng)
		start := time.Now()
		c, err := parseFacelets(state)
		if err != "" {
			failed++
			continue
		}
		sol := solveState(c, 22, 10*time.Second)
		elapsed := time.Since(start)
		total += elapsed
		if elapsed > worst {
			worst = elapsed
		}
		if sol == nil {
			failed++
			continue
		}
		check := state
		for _, m := range sol {
			check = applyPerm(check, faceletPm[m])
		}
		if check == solvedFacelets {
			passed++
		} else {
			failed++
		}
	}
	avg := int64(0)
	if n > 0 {
		avg = total.Milliseconds() / int64(n)
	}
	return map[string]interface{}{
		"passed":    passed,
		"failed":    failed,
		"avgMs":     avg,
		"worstMs":   worst.Milliseconds(),
	}
}

func main() {
	initGeometry()
	initMoves()
	initCubieTables()
	initCubieMoves()
	initSliceTables()
	js.Global().Set("cubeSolver", js.ValueOf(jsObj()))
	select {}
}

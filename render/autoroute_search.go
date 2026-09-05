package render

import (
	"container/heap"
	"math"
)

// ---------------------------------------------------------------------------
// A* over a navGrid, plus the string-pull that turns the raw cell path into a
// short list of waypoints a helmsman would actually steer.
// ---------------------------------------------------------------------------

// neighbourOffsets is a 16-way move set: the usual 8 plus the knight moves.
// Eight neighbours quantise every leg to a multiple of 45°, which on a long
// open-water leg shows up as a visible staircase and adds up to 8% to the
// distance; sixteen brings the worst-case heading error down to ~14° and the
// string-pull below removes most of what is left.
var neighbourOffsets = [16][2]int{
	{1, 0}, {-1, 0}, {0, 1}, {0, -1},
	{1, 1}, {1, -1}, {-1, 1}, {-1, -1},
	{1, 2}, {2, 1}, {2, -1}, {1, -2},
	{-1, -2}, {-2, -1}, {-2, 1}, {-1, 2},
}

// nodeQueue is the A* open set.
type nodeQueue struct {
	items []int
	f     []float64
	pos   []int32 // cell -> index in items, -1 when absent
}

func (q *nodeQueue) Len() int           { return len(q.items) }
func (q *nodeQueue) Less(i, j int) bool { return q.f[q.items[i]] < q.f[q.items[j]] }
func (q *nodeQueue) Swap(i, j int) {
	q.items[i], q.items[j] = q.items[j], q.items[i]
	q.pos[q.items[i]] = int32(i)
	q.pos[q.items[j]] = int32(j)
}
func (q *nodeQueue) Push(x any) {
	v := x.(int)
	q.pos[v] = int32(len(q.items))
	q.items = append(q.items, v)
}
func (q *nodeQueue) Pop() any {
	old := q.items
	n := len(old)
	v := old[n-1]
	q.items = old[:n-1]
	q.pos[v] = -1
	return v
}

// findPath runs A* from start to goal over the grid's cost multipliers and
// returns the cell indices of the path (start and goal included), or nil when
// the goal is unreachable. Costs are metres: a move's cost is its length times
// the mean multiplier of the cells it crosses, so the heuristic (straight-line
// metres, multiplier 1) never overestimates and the result is optimal.
func (g *navGrid) findPath(start, goal int) []int {
	n := g.nx * g.ny
	if start < 0 || goal < 0 || start >= n || goal >= n {
		return nil
	}
	if !g.passable(start) || !g.passable(goal) {
		return nil
	}
	if start == goal {
		return []int{start}
	}

	gScore := make([]float64, n)
	for i := range gScore {
		gScore[i] = math.Inf(1)
	}
	came := make([]int32, n)
	for i := range came {
		came[i] = -1
	}
	closed := make([]bool, n)

	gx, gy := goal%g.nx, goal/g.nx
	h := func(i int) float64 {
		dx := float64(i%g.nx-gx) * g.mx
		dy := float64(i/g.nx-gy) * g.my
		return math.Hypot(dx, dy)
	}

	q := &nodeQueue{f: make([]float64, n), pos: make([]int32, n)}
	for i := range q.pos {
		q.pos[i] = -1
	}
	gScore[start] = 0
	q.f[start] = h(start)
	heap.Push(q, start)

	for q.Len() > 0 {
		cur := heap.Pop(q).(int)
		if cur == goal {
			return reconstruct(came, start, goal)
		}
		if closed[cur] {
			continue
		}
		closed[cur] = true
		cx, cy := cur%g.nx, cur/g.nx
		for _, o := range neighbourOffsets {
			nxi, nyi := cx+o[0], cy+o[1]
			if nxi < 0 || nyi < 0 || nxi >= g.nx || nyi >= g.ny {
				continue
			}
			nb := nyi*g.nx + nxi
			if closed[nb] || !g.passable(nb) {
				continue
			}
			step, ok := g.moveCost(cur, nb)
			if !ok {
				continue
			}
			tentative := gScore[cur] + step
			if tentative >= gScore[nb] {
				continue
			}
			gScore[nb] = tentative
			came[nb] = int32(cur)
			q.f[nb] = tentative + h(nb)
			if q.pos[nb] >= 0 {
				heap.Fix(q, int(q.pos[nb]))
			} else {
				heap.Push(q, nb)
			}
		}
	}
	return nil
}

func reconstruct(came []int32, start, goal int) []int {
	path := []int{goal}
	for cur := goal; cur != start; {
		prev := came[cur]
		if prev < 0 {
			return nil
		}
		cur = int(prev)
		path = append(path, cur)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// moveCost prices a single move: its length in metres times the mean cost
// multiplier of every cell the straight line between the two centres passes
// through. It reports false when any of those cells is impassable — which is
// what stops a diagonal or knight move from slipping through the corner
// between two rocks.
func (g *navGrid) moveCost(a, b int) (float64, bool) {
	sum, count, ok := g.traverse(a, b, nil)
	if !ok {
		return 0, false
	}
	ax, ay := a%g.nx, a/g.nx
	bx, by := b%g.nx, b/g.nx
	length := math.Hypot(float64(bx-ax)*g.mx, float64(by-ay)*g.my)
	return length * (sum / float64(count)), true
}

// traverse walks the cells under the segment between two cell centres,
// accumulating their cost multipliers. It returns false as soon as it hits an
// impassable cell. When maxMult is non-nil, it also returns false on any cell
// whose multiplier exceeds it — that is how the string-pull refuses to swap a
// careful dogleg for a shortcut through worse water.
func (g *navGrid) traverse(a, b int, maxMult *float64) (sum float64, count int, ok bool) {
	ax, ay := a%g.nx, a/g.nx
	bx, by := b%g.nx, b/g.nx
	steps := maxInt(absInt(bx-ax), absInt(by-ay))
	if steps == 0 {
		c := float64(g.cost[a])
		if math.IsInf(c, 1) {
			return 0, 0, false
		}
		return c, 1, true
	}
	// Two samples per cell keeps the walk from stepping over a one-cell-wide
	// obstruction on a shallow-angle segment.
	samples := steps * 2
	prev := -1
	for s := 0; s <= samples; s++ {
		t := float64(s) / float64(samples)
		ix := int(math.Round(float64(ax) + t*float64(bx-ax)))
		iy := int(math.Round(float64(ay) + t*float64(by-ay)))
		if ix < 0 || iy < 0 || ix >= g.nx || iy >= g.ny {
			return 0, 0, false
		}
		i := iy*g.nx + ix
		if i == prev {
			continue
		}
		prev = i
		c := float64(g.cost[i])
		if math.IsInf(c, 1) {
			return 0, 0, false
		}
		if maxMult != nil && c > *maxMult {
			return 0, 0, false
		}
		sum += c
		count++
	}
	return sum, count, true
}

// pullTaut collapses a cell-by-cell A* path into the fewest waypoints that
// still describe it: from each kept point, reach as far ahead as a straight
// leg stays passable and no worse than the water the leg replaces. This is
// what removes the grid staircase, and unlike a plain Douglas-Peucker it can
// never shortcut a corner across a shoal, because every candidate leg is
// re-walked through the grid first.
//
// pullWindow bounds the look-ahead so a long open-water path stays linear-ish
// rather than quadratic in the number of cells.
const pullWindow = 512

func (g *navGrid) pullTaut(path []int) []int {
	if len(path) < 3 {
		return path
	}
	// A shortcut is allowed to be marginally worse than the sub-path it
	// replaces; without the slack, float noise in the mean blocks legs that
	// are visibly identical.
	const slack = 0.05
	out := []int{path[0]}
	i := 0
	for i < len(path)-1 {
		best := i + 1
		runMax := math.Max(float64(g.cost[path[i]]), float64(g.cost[path[i+1]]))
		for j := i + 2; j < len(path) && j-i <= pullWindow; j++ {
			runMax = math.Max(runMax, float64(g.cost[path[j]]))
			limit := runMax + slack
			if _, _, ok := g.traverse(path[i], path[j], &limit); !ok {
				break
			}
			best = j
		}
		out = append(out, path[best])
		i = best
	}
	return out
}

// thinToLimit drops the least significant waypoints until at most max remain,
// removing only points whose bypass leg is still safe. Returns the kept path
// and whether anything had to go.
func (g *navGrid) thinToLimit(path []int, max int) ([]int, bool) {
	if max < 2 || len(path) <= max {
		return path, false
	}
	pts := append([]int(nil), path...)
	removed := false
	for len(pts) > max {
		bestAt, bestDev := -1, math.Inf(1)
		for k := 1; k < len(pts)-1; k++ {
			runMax := math.Max(math.Max(float64(g.cost[pts[k-1]]), float64(g.cost[pts[k]])), float64(g.cost[pts[k+1]]))
			limit := runMax + 0.05
			if _, _, ok := g.traverse(pts[k-1], pts[k+1], &limit); !ok {
				continue
			}
			if dev := g.deviationM(pts[k-1], pts[k], pts[k+1]); dev < bestDev {
				bestDev, bestAt = dev, k
			}
		}
		if bestAt < 0 {
			break // nothing else can go without cutting a corner
		}
		pts = append(pts[:bestAt], pts[bestAt+1:]...)
		removed = true
	}
	return pts, removed
}

// deviationM is the perpendicular distance in metres from cell m to the line
// through cells a and b.
func (g *navGrid) deviationM(a, m, b int) float64 {
	ax, ay := float64(a%g.nx)*g.mx, float64(a/g.nx)*g.my
	mx, my := float64(m%g.nx)*g.mx, float64(m/g.nx)*g.my
	bx, by := float64(b%g.nx)*g.mx, float64(b/g.nx)*g.my
	dx, dy := bx-ax, by-ay
	den := math.Hypot(dx, dy)
	if den == 0 {
		return math.Hypot(mx-ax, my-ay)
	}
	return math.Abs(dy*(mx-ax)-dx*(my-ay)) / den
}

// nearestPassable finds the closest passable cell to a target, searching
// outward in rings up to maxRadiusM. Used to snap a start or end point that
// lands on land, on a shoal, or inside the shore-clearance buffer.
func (g *navGrid) nearestPassable(i int, maxRadiusM float64) (int, bool) {
	if g.passable(i) {
		return i, true
	}
	cx, cy := i%g.nx, i/g.nx
	maxR := int(math.Ceil(maxRadiusM / math.Min(g.mx, g.my)))
	for r := 1; r <= maxR; r++ {
		best, bestD := -1, math.Inf(1)
		for iy := cy - r; iy <= cy+r; iy++ {
			for ix := cx - r; ix <= cx+r; ix++ {
				// Ring only — the interior was covered by smaller r.
				if absInt(ix-cx) != r && absInt(iy-cy) != r {
					continue
				}
				if ix < 0 || iy < 0 || ix >= g.nx || iy >= g.ny {
					continue
				}
				j := iy*g.nx + ix
				if !g.passable(j) {
					continue
				}
				d := math.Hypot(float64(ix-cx)*g.mx, float64(iy-cy)*g.my)
				if d < bestD {
					bestD, best = d, j
				}
			}
		}
		if best >= 0 {
			return best, true
		}
	}
	return 0, false
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

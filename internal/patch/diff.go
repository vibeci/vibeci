package patch

// edit is one line of an edit script.
type edit struct {
	op   byte // ' ', '-', '+'
	a, b int  // line indices in the old (' ', '-') and new (' ', '+') file
}

// maxEditDistance bounds Myers' O(D²) memory; larger differences are
// emitted as one replacement of the differing middle part.
const maxEditDistance = 2500

// diffLines returns an edit script turning a into b.
func diffLines(a, b []string) []edit {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	edits := make([]edit, 0, len(a)+len(b)-pre-suf)
	for i := 0; i < pre; i++ {
		edits = append(edits, edit{' ', i, i})
	}
	for _, e := range myers(a[pre:len(a)-suf], b[pre:len(b)-suf]) {
		if e.op != '+' {
			e.a += pre
		}
		if e.op != '-' {
			e.b += pre
		}
		edits = append(edits, e)
	}
	for i := 0; i < suf; i++ {
		edits = append(edits, edit{' ', len(a) - suf + i, len(b) - suf + i})
	}
	return edits
}

// myers is Myers' O((N+M)D) shortest edit script.
func myers(a, b []string) []edit {
	n, m := len(a), len(b)
	replace := func() []edit {
		out := make([]edit, 0, n+m)
		for i := 0; i < n; i++ {
			out = append(out, edit{'-', i, -1})
		}
		for j := 0; j < m; j++ {
			out = append(out, edit{'+', -1, j})
		}
		return out
	}
	if n == 0 || m == 0 {
		return replace()
	}
	maxD := min(n+m, maxEditDistance)
	off := maxD + 1
	v := make([]int32, 2*maxD+3)
	var trace [][]int32 // trace[d][k+d] = furthest x on diagonal k after step d
	for d := 0; d <= maxD; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = int(v[off+k+1])
			} else {
				x = int(v[off+k-1]) + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = int32(x)
			if x >= n && y >= m {
				snap := make([]int32, 2*d+1)
				copy(snap, v[off-d:off+d+1])
				trace = append(trace, snap)
				return backtrack(trace, n, m)
			}
		}
		snap := make([]int32, 2*d+1)
		copy(snap, v[off-d:off+d+1])
		trace = append(trace, snap)
	}
	return replace()
}

func backtrack(trace [][]int32, n, m int) []edit {
	var out []edit
	x, y := n, m
	for d := len(trace) - 1; d > 0; d-- {
		prev := trace[d-1] // covers diagonals -(d-1)..d-1
		get := func(k int) int { return int(prev[k+d-1]) }
		k := x - y
		var pk int
		if k == -d || (k != d && get(k-1) < get(k+1)) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := get(pk)
		py := px - pk
		mx, my := px+1, py // after a deletion of a[px]
		if pk == k+1 {
			mx, my = px, py+1 // after an insertion of b[py]
		}
		for x > mx && y > my {
			out = append(out, edit{' ', x - 1, y - 1})
			x--
			y--
		}
		if pk == k+1 {
			out = append(out, edit{'+', -1, py})
		} else {
			out = append(out, edit{'-', px, -1})
		}
		x, y = px, py
	}
	for x > 0 && y > 0 {
		out = append(out, edit{' ', x - 1, y - 1})
		x--
		y--
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Diff returns the hunks turning before into after with ctx lines of
// context, written in style s. Content missing a final newline gets the
// "\ No newline at end of file" markers.
func Diff(before, after []byte, ctx int, s Style) []*Hunk {
	ta, tb := splitText(before), splitText(after)
	// The last line without a newline must not equal the same text with
	// one; a line never contains '\n', so it marks the difference.
	ka, kb := ta.lines, tb.lines
	if !ta.eol && len(ka) > 0 {
		ka = append(append([]string(nil), ka[:len(ka)-1]...), ka[len(ka)-1]+"\n")
	}
	if !tb.eol && len(kb) > 0 {
		kb = append(append([]string(nil), kb[:len(kb)-1]...), kb[len(kb)-1]+"\n")
	}
	edits := diffLines(ka, kb)
	return hunks(edits, ta, tb, ctx, s)
}

func hunks(edits []edit, ta, tb text, ctx int, s Style) []*Hunk {
	n := len(edits)
	aBefore := make([]int, n+1) // old lines before edit i
	bBefore := make([]int, n+1)
	for i, e := range edits {
		aBefore[i+1], bBefore[i+1] = aBefore[i], bBefore[i]
		if e.op != '+' {
			aBefore[i+1]++
		}
		if e.op != '-' {
			bBefore[i+1]++
		}
	}
	var out []*Hunk
	for i := 0; i < n; {
		for i < n && edits[i].op == ' ' {
			i++
		}
		if i >= n {
			break
		}
		last := i
		j := i
		for j < n {
			if edits[j].op != ' ' {
				last = j
				j++
				continue
			}
			k := j
			for k < n && edits[k].op == ' ' {
				k++
			}
			if k >= n || k-j > 2*ctx {
				break
			}
			j = k
		}
		start, end := max(0, i-ctx), min(n, last+1+ctx)
		h := &Hunk{}
		for _, e := range edits[start:end] {
			var l Line
			switch e.op {
			case ' ':
				l = Line{Op: ' ', Text: ta.lines[e.a], NoNewline: e.a == len(ta.lines)-1 && !ta.eol}
				h.OldCount++
				h.NewCount++
			case '-':
				l = Line{Op: '-', Text: ta.lines[e.a], NoNewline: e.a == len(ta.lines)-1 && !ta.eol}
				h.OldCount++
			case '+':
				l = Line{Op: '+', Text: tb.lines[e.b], NoNewline: e.b == len(tb.lines)-1 && !tb.eol}
				h.NewCount++
			}
			h.Lines = append(h.Lines, l)
		}
		h.OldStart, h.NewStart = aBefore[start], bBefore[start]
		if h.OldCount > 0 {
			h.OldStart++
		}
		if h.NewCount > 0 {
			h.NewStart++
		}
		h.Section = sectionFor(ta.lines, aBefore[start], s)
		out = append(out, h)
		i = end
	}
	return out
}

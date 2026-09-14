package w

type S struct {
	F   int
	Arr [4]int
	M   map[string]int
}

var G int

func run(s *S, p *int, i int) {
	x := 1
	x += 2
	x++
	s.F = 3
	s.Arr[i] = 4
	*p = 5
	*p += 1
	G = x
	a, b := x, x
	a, b = b, a
	s.M["k"] = 6
	_ = a
	_ = b
}

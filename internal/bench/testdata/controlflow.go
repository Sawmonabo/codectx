// Package controlflow gives the dependence-core benchmarks one Go function per
// group of the control-flow and definition forms the Go lowering's contract
// names: branches, the loop forms, labels, both switch forms, select with and
// without default, goto, defer and panic, go, short-circuit operators, var and
// const declarations, field, index and indirect targets, a method with named
// results and a bare return, and closures. It is a benchmark input, not a
// proof of coverage: the lowering's golden tests are that.
package controlflow

import (
	"errors"
	"fmt"
)

func branches(x, y int) int {
	if x > y {
		x, y = y, x
	} else if x == y {
		return 0
	} else {
		y++
	}
	return y - x
}

func loops(items []int) (sum int) {
	for i := 0; i < len(items); i++ {
		sum += items[i]
	}
	for sum > 100 {
		sum /= 2
	}
	for i, v := range items {
		if v < 0 {
			continue
		}
		sum += i * v
	}
	for {
		if sum%2 == 0 {
			break
		}
		sum++
	}
	return sum
}

func labelled(grid [][]int, want int) (int, int) {
outer:
	for r, row := range grid {
		for c, v := range row {
			switch {
			case v == want:
				return r, c
			case v > want:
				continue outer
			case v < 0:
				break outer
			}
		}
	}
	return -1, -1
}

func switches(v any, n int) string {
	switch n {
	case 0:
		fallthrough
	case 1:
		n++
	default:
		n--
	}
	switch t := v.(type) {
	case int:
		return fmt.Sprint(t + n)
	case string:
		return t
	}
	return ""
}

func channels(a, b <-chan int, done chan struct{}) int {
	total := 0
	for {
		select {
		case v := <-a:
			total += v
		case v, ok := <-b:
			if !ok {
				return total
			}
			total -= v
		case <-done:
			return total
		}
	}
}

func jumps(n int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("recovered")
		}
	}()
	i := 0
loop:
	if i < n {
		i++
		goto loop
	}
	if i > 1000 {
		panic("too far")
	}
	return nil
}

func closures(xs []int) func() int {
	total := 0
	add := func(v int) { total += v }
	for _, x := range xs {
		add(x)
	}
	return func() int { return total }
}

func forever(ch chan int) {
	for {
		ch <- 1
	}
}

// T is the receiver of the method form.
type T struct{ n int }

func (t *T) method(xs []int, p *int) (n int, ok bool) {
	var total int
	const limit = 10
	if len(xs) > 0 && xs[0] > 0 || p != nil {
		total = limit
	}
	t.n = total
	xs[0] = total
	*p = total
	n, ok = total, total > 0
	return
}

func concurrent(ch chan int, stop <-chan struct{}) int {
	done := make(chan int, 1)
	go func() { done <- 1 }()
	select {
	case v := <-ch:
		return v
	case ch <- 2:
	case <-stop:
		return -1
	default:
	}
	return <-done
}

func blocked() {
	select {}
}

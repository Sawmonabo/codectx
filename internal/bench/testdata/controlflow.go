// Package controlflow gives the dependence-core benchmarks one Go function per
// control-flow form the lowering handles, so every path of it is exercised.
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

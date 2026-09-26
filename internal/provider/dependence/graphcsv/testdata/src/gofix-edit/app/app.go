package app

import "example.com/fix/helper"

type Counter struct{ N int }

var Total int

func Run(c *Counter, flag bool) int {
	x := 1
	if flag {
		x = helper.Scale(x)
		c.N = x
	} else {
		x += 3
		c.N = x
	}
	Total = x
	return x
}

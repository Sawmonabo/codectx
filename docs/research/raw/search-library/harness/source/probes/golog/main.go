// golog prints x and math.Log(x) as hex bits: the IDF argument range plus edges.
package main

import (
	"fmt"
	"math"
)

func main() {
	xs := []float64{1, 2, 0.5, math.Sqrt2 / 2, 7.07106781186547524401e-01, 2 * 7.07106781186547524401e-01, math.Nextafter(2*7.07106781186547524401e-01, 0), math.Nextafter(2*7.07106781186547524401e-01, 3)}
	for n := int64(1); n <= 600000; n = n*3 + 1 {
		for df := int64(0); df <= n; df = df*2 + 1 {
			xs = append(xs, 1+(float64(n)-float64(df)+0.5)/(float64(df)+0.5))
		}
	}
	s := uint64(88172645463325252)
	for i := 0; i < 200000; i++ {
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		xs = append(xs, 1+float64(s%1000000007)/float64(1+s%9973))
	}
	for _, x := range xs {
		fmt.Printf("%016x %016x\n", math.Float64bits(x), math.Float64bits(math.Log(x)))
	}
}

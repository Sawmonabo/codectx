// Package sample exercises the Go query pack.
package sample

import (
	"fmt"
	str "strings"
)

// greeting holds a non-ASCII literal so later byte ranges differ from rune counts: héllo → 日本.
const greeting = "héllo → 日本"

// Server is a struct with a nested method.
type Server struct {
	Name string
	port int
}

// Handler is an interface.
type Handler interface {
	Serve(name string) error
}

// Start runs the server and calls helpers.
func (s *Server) Start() error {
	inner := func() { fmt.Println(str.ToUpper(greeting)) }
	inner()
	return helper(s.Name)
}

func helper(name string) error { return nil }

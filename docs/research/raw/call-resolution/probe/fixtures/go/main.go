package main

type Animal interface {
	Sound()
}

type Dog struct{}
type Cat struct{}

func (Dog) Sound() {}
func (Cat) Sound() {}

func speak(a Animal) {
	a.Sound()
}

func direct(d Dog) {
	d.Sound()
}

func main() {
	speak(Dog{})
	speak(Cat{})
	direct(Dog{})
}

package main

type Animal interface { // @tanimal
	Sound() // @spec
}

type Dog struct{} // @tdog
type Cat struct{} // @tcat

func (Dog) Sound() {} // @dog
func (Cat) Sound() {} // @cat

func speak(a Animal) { // @fspeak
	a.Sound() // @speak
}

func direct(d Dog) { // @fdirect
	d.Sound() // @direct
}

func main() {
	speak(Dog{})  // @m1
	speak(Cat{})  // @m2
	direct(Dog{}) // @m3
}

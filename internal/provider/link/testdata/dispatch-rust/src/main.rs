trait Animal { // @tanimal
    fn sound(&self); // @spec
}

struct Dog; // @tdog
struct Cat; // @tcat

impl Animal for Dog {
    fn sound(&self) {} // @dog
}

impl Animal for Cat {
    fn sound(&self) {} // @cat
}

fn speak(a: &dyn Animal) { // @fspeak
    a.sound() // @speak
}

fn speak_g<T: Animal>(a: &T) { // @fspeakg
    a.sound() // @speakg
}

fn direct(d: &Dog) { // @fdirect
    d.sound() // @direct
}

fn main() {
    speak(&Dog); // @m1
    speak(&Cat); // @m2
    speak_g(&Dog); // @m3
    speak_g(&Cat); // @m4
    direct(&Dog); // @m5
}

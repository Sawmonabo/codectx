trait Animal {
    fn sound(&self);
}

struct Dog;
struct Cat;

impl Animal for Dog {
    fn sound(&self) {}
}

impl Animal for Cat {
    fn sound(&self) {}
}

fn speak(a: &dyn Animal) {
    a.sound()
}

fn speak_g<T: Animal>(a: &T) {
    a.sound()
}

fn direct(d: &Dog) {
    d.sound()
}

fn main() {
    speak(&Dog);
    speak(&Cat);
    speak_g(&Dog);
    speak_g(&Cat);
    direct(&Dog);
}

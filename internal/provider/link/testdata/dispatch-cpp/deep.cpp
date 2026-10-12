struct Animal { // @tanimal
    virtual ~Animal() {}
    virtual void sound() {} // @asound
};

struct Dog : Animal { // @tdog
    void sound() override {} // @dsound
};

struct Puppy : Dog { // @tpuppy
    void sound() override {} // @psound
};

void speak(Animal& a) { // @fspeak
    a.sound(); // @speak
}

void walk(Dog& d) { // @fwalk
    d.sound(); // @walk
}

void exact(Animal& a) { // @fexact
    a.Animal::sound(); // @exact
}

int main() {
    Puppy p; // @p
    speak(p); // @m1
    walk(p); // @m2
    exact(p); // @m3
    Dog().sound(); // @m4
    return 0;
}

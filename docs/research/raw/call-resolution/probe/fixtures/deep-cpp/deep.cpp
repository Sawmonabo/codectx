struct Animal {
    virtual ~Animal() {}
    virtual void sound() {}
};

struct Dog : Animal {
    void sound() override {}
};

struct Puppy : Dog {
    void sound() override {}
};

void speak(Animal& a) {
    a.sound();
}

void walk(Dog& d) {
    d.sound();
}

void exact(Animal& a) {
    a.Animal::sound();
}

int main() {
    Puppy p;
    speak(p);
    walk(p);
    exact(p);
    return 0;
}

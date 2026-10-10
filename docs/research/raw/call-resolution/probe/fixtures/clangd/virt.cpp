struct Animal {
    virtual ~Animal() {}
    virtual void sound() {}
};

struct Dog : Animal {
    void sound() override {}
};

struct Cat : Animal {
    void sound() override {}
};

void speak(Animal& a) {
    a.sound();
}

void direct(Dog& d) {
    d.sound();
}

template <typename T>
void speak_t(T& a) {
    a.sound();
}

int main() {
    Dog d;
    Cat c;
    speak(d);
    speak(c);
    direct(d);
    speak_t(d);
    speak_t(c);
    return 0;
}

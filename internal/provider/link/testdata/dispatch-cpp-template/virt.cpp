struct Animal { // @tanimal
    virtual ~Animal() {}
    virtual void sound() {} // @asound
};

struct Dog : Animal { // @tdog
    void sound() override {} // @dsound
};

struct Cat : Animal { // @tcat
    void sound() override {} // @csound
};

void speak(Animal& a) { // @fspeak
    a.sound(); // @speak
}

void direct(Dog& d) { // @fdirect
    d.sound(); // @direct
}

template <typename T>
void speak_t(T& a) { // @fspeakt
    a.sound(); // @speakt
}

int main() {
    Dog d;
    Cat c;
    speak(d); // @m1
    speak(c); // @m2
    direct(d); // @m3
    speak_t(d); // @m4
    speak_t(c); // @m5
    return 0;
}

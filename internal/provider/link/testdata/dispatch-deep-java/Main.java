class Animal { // @tanimal
    void sound() {} // @asound
}

class Dog extends Animal { // @tdog
    @Override
    void sound() {} // @dsound
}

class Puppy extends Dog { // @tpuppy
    @Override
    void sound() { // @psound
        super.sound(); // @super
    }
}

public class Main {
    static void speak(Animal a) { // @fspeak
        a.sound(); // @speak
    }

    static void walk(Dog d) { // @fwalk
        d.sound(); // @walk
    }

    public static void main(String[] args) {
        speak(new Puppy()); // @m1
        walk(new Puppy()); // @m2
        new Dog().sound(); // @m3
    }
}

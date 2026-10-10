class Animal {
    void sound() {}
}

class Dog extends Animal {
    @Override
    void sound() {}
}

class Puppy extends Dog {
    @Override
    void sound() {
        super.sound();
    }
}

public class Main {
    static void speak(Animal a) {
        a.sound();
    }

    static void walk(Dog d) {
        d.sound();
    }

    public static void main(String[] args) {
        speak(new Puppy());
        walk(new Puppy());
    }
}

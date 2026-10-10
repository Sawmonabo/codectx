class Animal { // @tanimal
    void sound() {} // @asound
}

class Dog extends Animal { // @tdog
    @Override
    void sound() {} // @dsound
}

class Cat extends Animal { // @tcat
    @Override
    void sound() {} // @csound
}

interface Speaker { // @tspeaker
    void talk(); // @stalk
}

class Parrot implements Speaker { // @tparrot
    public void talk() {} // @ptalk
}

class Robot implements Speaker { // @trobot
    public void talk() {} // @rtalk
}

public class Main {
    static void speak(Animal a) { // @fspeak
        a.sound(); // @speak
    }

    static void direct(Dog d) { // @fdirect
        d.sound(); // @direct
    }

    static void chat(Speaker s) { // @fchat
        s.talk(); // @chat
    }

    public static void main(String[] args) {
        speak(new Dog()); // @m1
        speak(new Cat()); // @m2
        direct(new Dog()); // @m3
        chat(new Parrot()); // @m4
        chat(new Robot()); // @m5
    }
}

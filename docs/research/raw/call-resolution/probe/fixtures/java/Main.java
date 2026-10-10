class Animal {
    void sound() {}
}

class Dog extends Animal {
    @Override
    void sound() {}
}

class Cat extends Animal {
    @Override
    void sound() {}
}

interface Speaker {
    void talk();
}

class Parrot implements Speaker {
    public void talk() {}
}

class Robot implements Speaker {
    public void talk() {}
}

public class Main {
    static void speak(Animal a) {
        a.sound();
    }

    static void direct(Dog d) {
        d.sound();
    }

    static void chat(Speaker s) {
        s.talk();
    }

    public static void main(String[] args) {
        speak(new Dog());
        speak(new Cat());
        direct(new Dog());
        chat(new Parrot());
        chat(new Robot());
    }
}

class Animal {
    sound(): void {}
}

class Dog extends Animal {
    sound(): void {}
}

class Cat extends Animal {
    sound(): void {}
}

function speak(a: Animal): void {
    a.sound();
}

function direct(d: Dog): void {
    d.sound();
}

interface Speaker {
    talk(): void;
}

class Parrot implements Speaker {
    talk(): void {}
}

class Robot implements Speaker {
    talk(): void {}
}

function chat(s: Speaker): void {
    s.talk();
}

function main(): void {
    speak(new Dog());
    speak(new Cat());
    direct(new Dog());
    chat(new Parrot());
    chat(new Robot());
}

main();

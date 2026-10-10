class Animal {
    sound(): void {}
}

class Dog extends Animal {
    sound(): void {}
}

class Puppy extends Dog {
    sound(): void {
        super.sound();
    }
}

function speak(a: Animal): void {
    a.sound();
}

function walk(d: Dog): void {
    d.sound();
}

speak(new Puppy());
walk(new Puppy());

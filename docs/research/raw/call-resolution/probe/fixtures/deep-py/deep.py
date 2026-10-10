class Animal:
    def sound(self):
        ...


class Dog(Animal):
    def sound(self):
        ...


class Puppy(Dog):
    def sound(self):
        super().sound()


def speak(a: Animal):
    a.sound()


def walk(d: Dog):
    d.sound()


speak(Puppy())
walk(Puppy())

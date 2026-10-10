class Animal:
    def sound(self): ...


class Dog(Animal):
    def sound(self): ...


class Cat(Animal):
    def sound(self): ...


def speak(a: Animal):
    a.sound()

class Animal:
    def sound(self):
        ...


class Dog(Animal):
    def sound(self):
        ...


class Cat(Animal):
    def sound(self):
        ...


def speak(a: Animal):
    a.sound()


def speak_u(a):
    a.sound()


def direct(d: Dog):
    d.sound()


def main():
    speak(Dog())
    speak(Cat())
    speak_u(Dog())
    direct(Dog())

from zoo import Animal, Dog


def speak(a: Animal):
    a.sound()


def untyped(x):
    x.sound()


Dog().sound()

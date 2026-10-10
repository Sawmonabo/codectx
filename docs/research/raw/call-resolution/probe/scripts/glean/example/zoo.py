class Base:
    def sound(self) -> str:
        return "..."


class Animal(Base):
    pass


class Dog(Animal):
    def sound(self) -> str:
        return "woof"

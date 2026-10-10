package zoo;

class Animal { void sound() {} }
class Dog extends Animal { void sound() {} }
class Cat extends Animal { void sound() {} }

public class Main {
  static void speak(Animal a) { a.sound(); }

  public static void main(String[] args) {
    speak(new Dog());
    speak(new Cat());
    new Dog().sound();
  }
}

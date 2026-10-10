package zoo;

interface Animal { void sound(); }
class Dog implements Animal { public void sound() {} }
class Cat implements Animal { public void sound() {} }

public class Main {
  static void speak(Animal a) { a.sound(); }

  public static void main(String[] args) {
    speak(new Dog());
    speak(new Cat());
    new Dog().sound();
  }
}

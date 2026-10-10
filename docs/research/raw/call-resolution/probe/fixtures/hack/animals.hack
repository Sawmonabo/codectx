class Animal {
  public function sound(): void {}
}

class Dog extends Animal {
  <<__Override>>
  public function sound(): void {}
}

class Cat extends Animal {
  <<__Override>>
  public function sound(): void {}
}

function speak(Animal $a): void {
  $a->sound();
}

function direct(Dog $d): void {
  $d->sound();
}

function dyn(dynamic $x): void {
  $x->sound();
}

<<__EntryPoint>>
function main(): void {
  speak(new Dog());
  speak(new Cat());
  direct(new Dog());
  dyn(new Cat());
}

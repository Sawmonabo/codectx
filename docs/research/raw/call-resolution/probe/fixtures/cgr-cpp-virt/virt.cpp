struct Animal { virtual ~Animal() {} virtual void sound() {} };
struct Dog : Animal { void sound() override {} };
struct Cat : Animal { void sound() override {} };
void speak(Animal& a) { a.sound(); }
void direct(Dog& d) { d.sound(); }
int main() { Dog d; Cat c; speak(d); speak(c); direct(d); return 0; }

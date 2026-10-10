struct Dog { void sound() {} };
struct Cat { void sound() {} };
template <typename T> void speak_t(T& a) { a.sound(); }
void direct() { Dog d; d.sound(); }
int main() { Dog d; Cat c; speak_t(d); speak_t(c); direct(); return 0; }

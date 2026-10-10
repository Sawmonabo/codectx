void f(void) {}
void h(void) {}

void (*gfp)(void) = h;

void g(void) {
    void (*fp)(void) = f;
    fp();
}

void g2(void) {
    gfp();
}

void run(void (*cb)(void)) {
    cb();
}

void k(void) {
    run(f);
    f();
}

int main(void) {
    g();
    g2();
    k();
    return 0;
}

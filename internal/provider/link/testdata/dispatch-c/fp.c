void f(void) {} // @f
void h(void) {} // @h

void (*gfp)(void) = h;

void g(void) { // @g
    void (*fp)(void) = f;
    fp(); // @c1
}

void g2(void) { // @g2
    gfp(); // @c2
}

void run(void (*cb)(void)) { // @run
    cb(); // @c3
}

void k(void) { // @k
    run(f); // @c4
    f(); // @c5
}

int main(void) {
    g(); // @m1
    g2(); // @m2
    k(); // @m3
    return 0;
}

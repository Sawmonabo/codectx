struct S { int f; int arr[4]; };
int g;
void run(struct S *s, int *p, int i) {
  int x = 1; x += 2; x++; --x;
  s->f = 3; s->arr[i] = 4; (*p) = 5; *p += 1; g = x;
  int y = x = 7;
}

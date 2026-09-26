class S { int f; int[] arr = new int[4]; }
class W {
  static int g;
  void run(S s, int i) {
    int x = 1; x += 2; x++; --x;
    s.f = 3; s.arr[i] = 4; g = x;
    int y = x = 7;
  }
}

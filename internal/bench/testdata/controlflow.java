// One Java method per group of the control-flow and definition forms the
// Java lowering's contract names: branches and the conditional and logical
// operators, the loop forms, labelled break and continue, both switch forms
// and switch expressions with yield and guarded patterns, try/catch/finally
// with a multi-catch, try-with-resources, synchronized, assert, instanceof
// patterns, field and array writes, lambdas, anonymous and local classes,
// initializers, records and enums. It is a benchmark input, not a proof of
// coverage: the lowering's golden tests are that.
import java.io.Reader;
import java.util.List;
import java.util.function.IntSupplier;

class ControlFlow {
    static final int LIMIT = compute();
    int seed = LIMIT > 0 ? LIMIT : -LIMIT;

    static {
        if (LIMIT > 10) {
            System.out.println(LIMIT);
        }
    }

    {
        seed += 1;
    }

    static int compute() {
        return 3;
    }

    int branches(int x, int y) {
        if (x > y) {
            int t = x;
            x = y;
            y = t;
        } else if (x == y) {
            return 0;
        } else {
            y++;
        }
        int z = x > 0 ? x : -x;
        return (y - x) * (z != 0 && y > 0 || x < 0 ? z : 1);
    }

    int loops(int[] items, List<Integer> list) {
        int sum = 0;
        for (int i = 0, j = items.length; i < j; i++, j--) {
            sum += items[i];
        }
        while (sum > 100) {
            sum /= 2;
        }
        do {
            sum--;
        } while (sum % 7 != 0);
        for (int v : list) {
            if (v < 0) {
                continue;
            }
            sum += v;
        }
        for (;;) {
            if (sum % 2 == 0) {
                break;
            }
            sum++;
        }
        return sum;
    }

    int labels(int[][] grid) {
        int found = -1;
        outer:
        for (int r = 0; r < grid.length; r++) {
            for (int c = 0; c < grid[r].length; c++) {
                if (grid[r][c] < 0) {
                    continue outer;
                }
                if (grid[r][c] == 0) {
                    found = r;
                    break outer;
                }
            }
        }
        block:
        {
            if (found < 0) {
                break block;
            }
            found *= 2;
        }
        return found;
    }

    int switches(int x, Object o) {
        int y = 0;
        switch (x) {
            case 1:
                y = 1;
            case 2:
            case 3:
                y += 2;
                break;
            default:
                y = 3;
        }
        switch (x) {
            case 4 -> y = 4;
            case 5, 6 -> {
                y = 5;
            }
            default -> throw new IllegalStateException();
        }
        int z = switch (o) {
            case Integer i when i > 0 -> i;
            case String s -> {
                for (int k = 0; k < s.length(); k++) {
                    if (s.charAt(k) == 'x') {
                        yield k;
                    }
                }
                yield s.length();
            }
            default -> 0;
        };
        return y + z;
    }

    int exceptions(String s, Reader in) throws Exception {
        int r = 0;
        try {
            r = Integer.parseInt(s);
        } catch (NumberFormatException | NullPointerException e) {
            r = -1;
        } catch (RuntimeException e) {
            throw e;
        } finally {
            r++;
        }
        try (Reader a = in; java.io.BufferedReader b = new java.io.BufferedReader(a)) {
            r += b.read();
        } catch (java.io.IOException e) {
            return r;
        }
        synchronized (this) {
            r *= 2;
        }
        assert r >= 0 : "negative " + r;
        return r;
    }

    Object patterns(Object o, int[] a, ControlFlow p) {
        if (!(o instanceof String s)) {
            return null;
        }
        a[0] = s.length();
        p.seed = a[0];
        this.seed++;
        return s;
    }

    IntSupplier closures(int base) {
        int n = base * 2;
        Runnable r = () -> System.out.println(n);
        r.run();
        class Local {
            int get() {
                return n + 1;
            }
        }
        Object anon = new Object() {
            int n = 1;

            @Override
            public String toString() {
                return "" + n + base;
            }
        };
        return () -> new Local().get() + anon.hashCode();
    }

    record Point(int x, int y) {
        Point {
            if (x < 0) {
                x = 0;
            }
        }
    }

    enum Color {
        RED(1), GREEN(2) {
            int shade() {
                return 3;
            }
        };

        final int code;

        Color(int code) {
            this.code = code;
        }

        int shade() {
            return code;
        }
    }

    interface Shape {
        int SIDES = 4;

        int area();
    }
}

/*
 * One C function per group of the control-flow and definition forms the
 * C-family lowering's contract names: branches, the loop forms including a
 * nonzero-literal loop, goto backward and forward, switch with default not
 * last and fallthrough, a case label nested in a loop (Duff's device),
 * short-circuit and conditional operators, embedded assignments, writes
 * through pointers, fields and indexes, block scoping, preprocessor
 * conditionals inside a body, and structured exceptions. It is a benchmark
 * input, not a proof of coverage: the lowering's golden tests are that.
 */
#include <stddef.h>

struct pair {
	int a, b;
};

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
	return y - x;
}

int loops(const int *items, int n) {
	int sum = 0;
	for (int i = 0; i < n; i++) {
		if (items[i] < 0) {
			continue;
		}
		sum += items[i];
	}
	while (sum > 100) {
		sum /= 2;
	}
	do {
		sum--;
	} while (sum % 7 != 0);
	while (1) {
		if (sum < 3) {
			break;
		}
		sum -= 3;
	}
	return sum;
}

int jumps(int x) {
again:
	x++;
	if (x < 10) {
		goto again;
	}
	if (x > 20) {
		goto out;
	}
	x *= 2;
out:
	return x;
}

int cases(int x) {
	int y = 0;
	switch (x) {
	default:
		y = 1;
	case 1:
		y += 2;
		break;
	case 2:
		return y;
	case 3: {
		int z = x * 2;
		y = z;
		break;
	}
	}
	return y;
}

void duff(char *to, const char *from, int count) {
	int n = (count + 7) / 8;
	switch (count % 8) {
	case 0:
		do {
			*to++ = *from++;
	case 7:
			*to++ = *from++;
	case 6:
			*to++ = *from++;
	case 5:
			*to++ = *from++;
	case 4:
			*to++ = *from++;
	case 3:
			*to++ = *from++;
	case 2:
			*to++ = *from++;
	case 1:
			*to++ = *from++;
		} while (--n > 0);
	}
}

int operators(int a, int b, int *p) {
	int c = a && b;
	int d = a || (b = a + 1);
	int e = c ? d : b;
	int r;
	while ((r = *p) != 0) {
		p++;
	}
	return c + d + e + r;
}

void writes(struct pair *pp, struct pair s, int *xs, int i) {
	pp->a = i;
	s.b = pp->a;
	xs[i] = s.b;
	*xs += 1;
}

int scopes(int x) {
	{
		int x = 1;
		x++;
	}
	static int calls = 0;
	calls++;
	return x + calls;
}

int configured(int x) {
	int y;
#ifdef FAST
	y = x * 2;
#elif defined(SMALL)
	y = x;
#else
	y = x + 1;
#endif
	return y;
}

#ifdef _MSC_VER
int guarded(int *p) {
	int r = 0;
	__try {
		if (!p) {
			__leave;
		}
		r = *p;
	} __finally {
		r++;
	}
	__try {
		r += *p;
	} __except (1) {
		r = -1;
	}
	return r;
}
#endif

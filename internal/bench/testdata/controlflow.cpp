// One C++ function per group of the control-flow and definition forms the
// C++ additions of the C-family lowering name: if and switch with
// init-statements and condition declarations, range for over a structured
// binding, try with typed catch clauses and catch (...), throw and rethrow,
// a function-try-block on a constructor with member initializers, methods,
// lambdas capturing by reference and by copy, a coroutine with co_await,
// co_yield and co_return, reference bindings, the alternative tokens `and`
// and `or`, and delete. It is a benchmark input, not a proof of coverage:
// the lowering's golden tests are that.
#include <coroutine>
#include <map>
#include <stdexcept>
#include <string>
#include <vector>

struct Counter {
	int total;
	std::vector<int> seen;

	Counter(int start) try : total(start), seen() {
		if (start < 0) {
			throw std::invalid_argument("negative");
		}
	} catch (const std::invalid_argument &) {
		total = 0;
	}

	int add(int x) {
		if (int y = x * 2; y > total) {
			total = y;
		}
		seen.push_back(x);
		return total;
	}
};

int ranges(const std::map<std::string, int> &m) {
	int sum = 0;
	for (const auto &[key, value] : m) {
		if (key.empty()) {
			continue;
		}
		sum += value;
	}
	for (int v : std::vector<int>{1, 2, 3}) {
		sum -= v;
	}
	return sum;
}

int handlers(int x) {
	try {
		if (x < 0) {
			throw std::runtime_error("negative");
		}
		x = std::stoi(std::to_string(x));
	} catch (const std::runtime_error &e) {
		return -1;
	} catch (const std::exception &) {
		throw;
	} catch (...) {
		return -2;
	}
	return x;
}

int selects(int x) {
	switch (int y = x % 4; y) {
	case 0:
		return x;
	case 1:
		x++;
	default:
		x--;
	}
	return x;
}

int lambdas(int a, int b) {
	auto byRef = [&a](int d) { a += d; };
	auto byCopy = [b]() mutable { b *= 2; return b; };
	auto all = [&]() { a = b; };
	byRef(1);
	int c = byCopy();
	all();
	return a + b + c;
}

struct Task {
	struct promise_type {
		Task get_return_object() { return {}; }
		std::suspend_never initial_suspend() { return {}; }
		std::suspend_never final_suspend() noexcept { return {}; }
		std::suspend_always yield_value(int) { return {}; }
		void return_value(int) {}
		void unhandled_exception() {}
	};
};

Task coroutine(int n) {
	for (int i = 0; i < n; i++) {
		co_yield i;
	}
	co_await std::suspend_always{};
	if (n > 10) {
		co_return n;
	}
	co_return 0;
}

int aliases(int *p, int n) {
	int x = 0;
	int &r = x;
	if (p and n > 0 or x) {
		r = *p;
	}
	for (auto &e : std::vector<int>{1, 2}) {
		e += n;
	}
	delete p;
	return x;
}

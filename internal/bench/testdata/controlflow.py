# One Python function per group of the control-flow and definition forms the
# Python lowering's contract names: branches and elif chains, the conditional
# and boolean operators, chained comparisons, while and for with else, break
# and continue, try with typed, grouped and bare except clauses, except*,
# else and finally, raise, with and async with over several items, match with
# guards, captures and an irrefutable case, assert, del, imports, global and
# nonlocal, assignment expressions, unpacking, attribute and subscript
# writes, decorators, lambdas, comprehensions and a class body. It is a
# benchmark input, not a proof of coverage: the lowering's golden tests are
# that.

import os.path as osp
from collections import deque

counter = 0


def branches(x, y):
    if x > y:
        x, y = y, x
    elif x == y:
        return 0
    else:
        y += 1
    z = x if x > 0 else -x
    return (y - x) * (z or 1) and 0 < z <= y


def loops(items, limit):
    total = 0
    i = 0
    while i < len(items):
        if items[i] < 0:
            i += 1
            continue
        total += items[i]
        if total > limit:
            break
        i += 1
    else:
        total = -total
    for k, v in enumerate(items):
        if v == limit:
            break
    else:
        k = -1
    while True:
        if total % 2 == 0:
            break
        total //= 2
    return total, k


def exceptions(path):
    handle = None
    try:
        handle = open(path)
        data = handle.read()
    except (OSError, ValueError) as err:
        data = str(err)
    except KeyError:
        raise
    except:
        data = None
    else:
        data = data.strip()
    finally:
        if handle is not None:
            handle.close()
    try:
        check(data)
    except* TypeError as group:
        data = group.exceptions
    return data


def contexts(first, second):
    with open(first) as a, open(second) as b:
        if a.readable():
            return a.read() + b.read()
        raise ValueError(first)


async def asynchronous(source):
    results = []
    async with source.lock() as guard:
        async for item in source:
            results.append(await guard.check(item))
    return results


def matching(command):
    match command.split():
        case ["go", direction] if direction in ("north", "south"):
            result = direction
        case ["drop", *objects]:
            result = len(objects)
        case {"action": action, **rest}:
            result = (action, rest)
        case Point(x=0, y=py) | Point(x=py, y=0):
            result = py
        case [first, _] as pair:
            result = (first, pair)
        case _:
            result = None
    return result


def statements(values, key):
    assert values, "values must not be empty"
    table = {}
    table[key] = values[0]
    table.update(first=values[0])
    del table[key]
    head, *tail = values
    first = second = head
    del first
    if (n := len(tail)) > 2:
        second = n
    return second, table, osp.join("a", "b"), deque(tail)


def scopes(seed):
    global counter
    counter += 1
    state = seed

    def bump(step=seed):
        nonlocal state
        state += step
        return state

    squares = [s * s for s in range(state) if s % 2]
    pairs = {s: t for s in squares for t in range(s) if t}
    total = sum(v for v in squares)
    found = any((last := v) > seed for v in squares)
    scale = lambda v, w=seed: v * w + state
    return bump, pairs, total, found, last, scale


def decorated(fn):
    @staticmethod
    def wrapper(*args, **kwargs):
        return fn(*args, **kwargs)

    return wrapper


class Shape:
    sides = 0
    names = [n for n in ("a", "b")]

    def __init__(self, sides):
        self.sides = sides

    def area(self):
        return sides if self.sides else 0

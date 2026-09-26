// One JavaScript function per control-flow form the lowering handles, so
// the dependence-core benchmarks exercise every path of it.

function branches(x, y) {
  if (x > y) {
    [x, y] = [y, x];
  } else if (x === y) {
    return 0;
  } else {
    y++;
  }
  const z = x > 0 ? x : -x;
  return (y - x) * (z || 1) + (y ?? 0) && z;
}

function loops(items, obj) {
  let sum = 0;
  for (let i = 0; i < items.length; i++) sum += items[i];
  while (sum > 100) sum /= 2;
  do {
    sum++;
  } while (sum % 2 !== 0);
  for (const key in obj) {
    if (!obj[key]) continue;
    sum += obj[key];
  }
  for (const { value = 0, ...rest } of items) {
    sum += value + Object.keys(rest).length;
  }
  return sum;
}

function labelled(grid, want) {
  outer: for (let r = 0; r < grid.length; r++) {
    for (let c = 0; c < grid[r].length; c++) {
      if (grid[r][c] === want) return [r, c];
      if (grid[r][c] > want) continue outer;
      if (grid[r][c] < 0) break outer;
    }
  }
  block: {
    if (want < 0) break block;
    want = -want;
  }
  return want;
}

function switches(n) {
  switch (n) {
    case 0:
    case 1:
      n++;
    case 2:
      n *= 2;
      break;
    default:
      n--;
  }
  return n;
}

function exceptions(read, path) {
  let result = null;
  try {
    result = read(path);
    if (!result) throw new Error("empty");
  } catch ({ message }) {
    result = message;
  } finally {
    path = null;
  }
  try {
    return read(result);
  } finally {
    result = undefined;
  }
}

function chains(config) {
  const port = config?.server?.port ?? 8080;
  const name = config && config.name || "default";
  return `${name}:${port}`;
}

function* generate(limit) {
  for (let i = 0; i < limit; i++) yield i;
}

async function fetchAll(urls, get) {
  const out = [];
  for await (const body of urls.map(async (u) => await get(u))) {
    out.push(body);
  }
  return out;
}

class Counter {
  #count = 0;
  increment(by = 1) {
    this.#count += by;
    return () => this.#count;
  }
}

function forever(emit) {
  for (;;) emit(1);
}

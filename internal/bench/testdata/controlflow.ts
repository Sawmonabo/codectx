// One TypeScript function per group of the forms the TypeScript lowering adds
// over JavaScript's: erased declarations and annotations, the transparent
// non-null, as, satisfies and angle-bracket assertion wrappers, parameter
// wrappers with defaults and parameter properties, enums, namespaces,
// import-equals declarations, abstract classes, decorators and overloads,
// next to the JavaScript control forms they wrap. It is a benchmark input,
// not a proof of coverage: the lowering's golden tests are that.

import fs = require("fs");
import type { Stats } from "fs";

interface Shape {
  area(): number;
}

type Pair<T> = [T, T];

declare const external: number;

enum Direction {
  Up = 1,
  Down = Up * 2,
  Left,
}

const enum Erased {
  A,
}

namespace Geometry {
  export const origin = 0;
  export function distance(a: number, b: number): number {
    return Math.abs(a - b) + origin;
  }
}

import distance = Geometry.distance;

function overloaded(x: string): string;
function overloaded(x: number): number;
function overloaded(x: string | number): string | number {
  if (typeof x === "string") return x.trim();
  return x * Direction.Down;
}

function wrappers(value: unknown, maybe?: string, count: number = 1): number {
  const n = value as number;
  const s = maybe!;
  const t = <string>value;
  const u = { n } satisfies { n: number };
  let total = n + count;
  for (let i = 0; i < count; i++) {
    total += s ? s.length : t.length;
  }
  return total + u.n;
}

function logged(target: unknown, key: string): void {
  console.log(key, target);
}

abstract class Base<T> implements Shape {
  protected readonly items: T[] = [];
  abstract area(): number;
  constructor(public name: string, private limit = 10) {}
  add(item: T): boolean {
    if (this.items.length >= this.limit) return false;
    this.items.push(item);
    return true;
  }
}

class Square extends Base<number> {
  side = 2;
  @logged
  area(): number {
    return this.side ** 2;
  }
}

function guarded(path: string, stats?: Stats): Pair<number> {
  try {
    const st = stats ?? fs.statSync(path);
    return [st.size, distance(0, st.size)];
  } catch (e: unknown) {
    return [0, external];
  } finally {
    new Square("s").add(1);
  }
}

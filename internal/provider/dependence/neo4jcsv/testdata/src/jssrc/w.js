let g = 0;
function run(s, i) {
  let x = 1; x += 2; x++; --x;
  s.f = 3; s.arr[i] = 4; g = x;
  let [a, b] = [x, x]; [a, b] = [b, a]; ({f: a} = s);
  let y = x = 7; s?.g; g ??= 1;
}

function helper(x) { return x + 1; }
function run(i) {
  const f = helper;
  return f(i);
}

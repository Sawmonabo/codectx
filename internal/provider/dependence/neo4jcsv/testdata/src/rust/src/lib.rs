pub struct S { pub f: i32, pub arr: [i32; 4] }
static mut G: i32 = 0;
pub fn run(s: &mut S, p: &mut i32, i: usize) {
    let mut x = 1; x += 2; x -= 1;
    s.f = 3; s.arr[i] = 4; *p = 5; *p += 1; unsafe { G = x; }
    let (mut a, mut b) = (x, x); (a, b) = (b, a);
    let _ = (a, b);
}

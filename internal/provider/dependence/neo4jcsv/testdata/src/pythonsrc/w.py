G = 0
class S:
    def __init__(self): self.f = 0; self.arr = [0]*4
def run(s, i):
    global G
    x = 1; x += 2
    s.f = 3; s.arr[i] = 4; G = x
    a, b = x, x; a, b = b, a
    y = x = 7
    d = {}; d["k"] = 6

package qual

import foo "example.com/qual/b"

type Word = foo.T

type Regs struct{ foo.Base }

func (Regs) Value() foo.T { return 0 }

type Getter interface{ Get() foo.T }

const N = foo.K

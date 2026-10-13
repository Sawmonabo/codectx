package qual

type Sized interface{ Size() Word }

type Buf interface{ Bytes() [N]byte }

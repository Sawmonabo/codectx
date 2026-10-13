package qual

type Sized interface{ Size() Word }

type Buf interface{ Bytes() [N]byte }

type Count = int16

type DotSized interface{ DotSize() DotWord }

type SameN interface{ N16() Same }

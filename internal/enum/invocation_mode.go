package enum

type InvocationMode int

const (
	UnknownMode InvocationMode = iota
	CompileMode
	PreprocessMode
	AssembleMode
	LinkMode
	DepOnlyMode
)

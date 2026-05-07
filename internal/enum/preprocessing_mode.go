package enum

type PreprocessingMode int

const (
	PreprocessUnknown PreprocessingMode = iota
	PreprocessRemote
	PreprocessLocal
)

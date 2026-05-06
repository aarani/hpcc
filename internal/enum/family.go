package enum

type Family int

const (
	UnknownFamily Family = iota
	GNUFamily
	MSVCFamily
)

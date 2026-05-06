package compiler

import "github.com/aarani/hpcc/internal/enum"

// Invocation: structured form of a compile command. One instance per compile command.
type Invocation struct {
	Inputs  []string
	Mode    enum.InvocationMode
	Include []string
	Defines map[string]string
	Std     string
	RawArgs []string
}

type InvocationResult struct {
	Stdout []byte
	Stderr []byte
	Err    error
}

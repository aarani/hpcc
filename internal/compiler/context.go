package compiler

import "github.com/aarani/hpcc/internal"

type Context struct {
	Compiler Compiler
	Config   internal.Config
}

func NewContext() *Context {
	return &Context{}
}

func (ctx *Context) SetCompiler(compiler Compiler) {
	ctx.Compiler = compiler
}

func (ctx *Context) GetCompiler() Compiler {
	return ctx.Compiler
}

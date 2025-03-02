package generator

import (
	"github.com/yassinebenaid/bunster/ast"
	"github.com/yassinebenaid/bunster/ir"
)

func (g *generator) handleTest(buf *InstructionBuffer, test ast.Test, ctx *context) {
	var cmdbuf InstructionBuffer

	cmdbuf.add(ir.CloneStreamManager{DeferDestroy: ctx.pipe == nil})
	g.handleRedirections(&cmdbuf, test.Redirections, ctx)

	g.handleTestExpression(&cmdbuf, test.Expr)

	*buf = append(*buf, ir.Closure(cmdbuf))
}

func (g *generator) handleTestExpression(buf *InstructionBuffer, test ast.Expression) {
	switch v := test.(type) {
	case ast.Binary:
		g.handleTestBinary(buf, v)
	}
}

func (g *generator) handleTestBinary(buf *InstructionBuffer, test ast.Binary) {
	switch test.Operator {
	case "=":
		l := g.handleExpression(buf, test.Left)
		r := g.handleExpression(buf, test.Right)

		buf.add(ir.Compare{
			Left:     l,
			Operator: "==",
			Right:    r,
		})
	}
}

package generator

import (
	"fmt"

	"github.com/yassinebenaid/bunster/ast"
	"github.com/yassinebenaid/bunster/ir"
)

type pipeContext struct {
	writer    string
	reader    string
	waitgroup string
	stderr    bool
}

type context struct {
	pipe  *pipeContext
	label string
}

type InstructionBuffer []ir.Instruction

func (ib *InstructionBuffer) add(ins ir.Instruction) {
	*ib = append(*ib, ins)
}

func Generate(script ast.Script) ir.Program {
	g := generator{}

	var buf InstructionBuffer
	for _, statement := range script {
		g.generate(&buf, statement, &context{})
	}

	return ir.Program{
		Instructions: buf,
	}
}

type generator struct {
	expressionsCount int
	scopesCount      int
}

func (g *generator) generate(buf *InstructionBuffer, statement ast.Statement, ctx *context) {
	switch v := statement.(type) {
	case ast.List:
		g.handleList(buf, v)
	case ast.Pipeline:
		g.handlePipeline(buf, v)
	case ast.Command:
		g.handleSimpleCommand(buf, v, ctx)
	case ast.ParameterAssignement:
		g.handleParameterAssignment(buf, v)
	case ast.Group:
		g.handleGroup(buf, v, ctx)
	case ast.SubShell:
		g.handleSubshell(buf, v, ctx)
	case ast.If:
		g.handleIf(buf, v, ctx)
	case ast.Break:
		g.handleBreak(buf, v)
	case ast.Continue:
		g.handleContinue(buf, v)
	case ast.Loop:
		g.handleLoop(buf, v, ctx)
	default:
		panic(fmt.Sprintf("Unsupported statement: %T", v))
	}
}

func (g *generator) handleList(buf *InstructionBuffer, l ast.List) {
	g.generate(buf, l.Left, &context{})

	var bodybuf InstructionBuffer
	g.generate(&bodybuf, l.Right, &context{})

	buf.add(ir.IfLastExitCode{
		Zero: l.Operator == "&&",
		Body: bodybuf,
	})
}

func (g *generator) handlePipeline(buf *InstructionBuffer, p ast.Pipeline) {
	var cmdbuf InstructionBuffer
	cmdbuf.add(ir.NewPipelineWaitgroup("pipelineWaitgroup"))

	for i, cmd := range p {
		if i < (len(p) - 1) { //last command doesn't need a pipe
			cmdbuf.add(ir.NewPipe{
				Writer: fmt.Sprintf("pipeWriter%d", i+1),
				Reader: fmt.Sprintf("pipeReader%d", i+1),
			})
		}

		var pc pipeContext
		if i == 0 {
			pc = pipeContext{
				writer: fmt.Sprintf("pipeWriter%d", i+1),
				stderr: cmd.Stderr,
			}
		} else if i == (len(p) - 1) {
			pc = pipeContext{
				reader: fmt.Sprintf("pipeReader%d", i),
			}
		} else {
			pc = pipeContext{
				writer: fmt.Sprintf("pipeWriter%d", i+1),
				reader: fmt.Sprintf("pipeReader%d", i),
				stderr: cmd.Stderr,
			}
		}

		pc.waitgroup = "pipelineWaitgroup"
		g.generate(&cmdbuf, cmd.Command, &context{pipe: &pc})
	}

	cmdbuf.add(ir.WaitPipelineWaitgroup("pipelineWaitgroup"))

	*buf = append(*buf, ir.Closure(cmdbuf))
}

func (g *generator) handleSimpleCommand(buf *InstructionBuffer, cmd ast.Command, ctx *context) {
	var cmdbuf InstructionBuffer

	cmdbuf.add(ir.Declare{Name: "commandName", Value: g.handleExpression(&cmdbuf, cmd.Name)})
	cmdbuf.add(ir.DeclareSlice{Name: "arguments"})

	for _, arg := range cmd.Args {
		cmdbuf.add(ir.Append{Name: "arguments", Value: g.handleExpression(&cmdbuf, arg)})
	}

	cmdbuf.add(ir.Declare{
		Name:  "command",
		Value: ir.InitCommand{Name: "commandName", Args: "arguments"},
	})

	for _, env := range cmd.Env {
		cmdbuf.add(ir.SetCmdEnv{
			Command: "command",
			Key:     env.Name,
			Value:   g.handleExpression(&cmdbuf, env.Value),
		})
	}

	cmdbuf.add(ir.CloneStreamManager{DeferDestroy: ctx.pipe == nil})
	g.handleRedirections(&cmdbuf, cmd.Redirections, ctx)
	cmdbuf.add(ir.SetStream{Name: "command.Stdin", Fd: ir.String("0")})
	cmdbuf.add(ir.SetStream{Name: "command.Stdout", Fd: ir.String("1")})
	cmdbuf.add(ir.SetStream{Name: "command.Stderr", Fd: ir.String("2")})

	if ctx.pipe != nil {
		cmdbuf.add(ir.StartCommand("command"))
		cmdbuf.add(ir.PushToPipelineWaitgroup{
			Waitgroup: ctx.pipe.waitgroup,
			Value: ir.Literal(`func() error {
			 	defer streamManager.Destroy()
				return command.Wait()
			}`),
		})
	} else {
		cmdbuf.add(ir.RunCommand("command"))
	}

	*buf = append(*buf, ir.Closure(cmdbuf))
}

func (g *generator) handleExpression(buf *InstructionBuffer, expression ast.Expression) ir.Instruction {
	g.expressionsCount++
	switch v := expression.(type) {
	case ast.Word:
		return ir.String(v)
	case ast.Number:
		return ir.String(v)
	case ast.Var:
		return ir.ReadVar(v)
	case ast.SpecialVar:
		return ir.ReadSpecialVar(v)
	case ast.QuotedString:
		var concat ir.Concat
		for _, expr := range v {
			concat = append(concat, g.handleExpression(buf, expr))
		}
		return concat
	case ast.UnquotedString:
		var concat ir.Concat
		for _, expr := range v {
			concat = append(concat, g.handleExpression(buf, expr))
		}
		return concat
	case ast.CommandSubstitution:
		return g.handleCommandSubstitution(buf, v)
	default:
		panic(fmt.Sprintf("Unsupported expression: %T", v))
	}
}

func (g *generator) handleRedirections(buf *InstructionBuffer, redirections []ast.Redirection, ctx *context) {

	// if we're inside a pipline, we need to connect the pipe to the command.(before any other redirection)
	if ctx.pipe != nil {
		if ctx.pipe.writer != "" {
			buf.add(ir.AddStream{Fd: "1", StreamName: ctx.pipe.writer, Proxy: true})

			if ctx.pipe.stderr {
				buf.add(ir.AddStream{Fd: "2", StreamName: ctx.pipe.writer, Proxy: true})
			}
		}

		if ctx.pipe.reader != "" {
			buf.add(ir.AddStream{Fd: "0", StreamName: ctx.pipe.reader})
		}
	}

	for i, redirection := range redirections {
		switch redirection.Method {
		case ">", ">|":
			buf.add(ir.OpenStream{
				Name:   fmt.Sprintf("stream%d", i),
				Target: g.handleExpression(buf, redirection.Dst),
				Mode:   ir.FLAG_WRITE,
				Label:  ctx.label,
			})
			buf.add(ir.AddStream{Fd: redirection.Src, StreamName: fmt.Sprintf("stream%d", i)})
		case ">>":
			buf.add(ir.OpenStream{
				Name:   fmt.Sprintf("stream%d", i),
				Target: g.handleExpression(buf, redirection.Dst),
				Mode:   ir.FLAG_APPEND,
				Label:  ctx.label,
			})
			buf.add(ir.AddStream{Fd: redirection.Src, StreamName: fmt.Sprintf("stream%d", i)})
		case "&>":
			buf.add(ir.OpenStream{
				Name:   fmt.Sprintf("stream%d", i),
				Target: g.handleExpression(buf, redirection.Dst),
				Mode:   ir.FLAG_WRITE,
				Label:  ctx.label,
			})
			buf.add(ir.AddStream{Fd: "1", StreamName: fmt.Sprintf("stream%d", i)})
			buf.add(ir.AddStream{Fd: "2", StreamName: fmt.Sprintf("stream%d", i)})
		case "&>>":
			buf.add(ir.OpenStream{
				Name:   fmt.Sprintf("stream%d", i),
				Target: g.handleExpression(buf, redirection.Dst),
				Mode:   ir.FLAG_APPEND,
				Label:  ctx.label,
			})
			buf.add(ir.AddStream{Fd: "1", StreamName: fmt.Sprintf("stream%d", i)})
			buf.add(ir.AddStream{Fd: "2", StreamName: fmt.Sprintf("stream%d", i)})
		case ">&", "<&":
			if redirection.Dst == nil && redirection.Close {
				buf.add(ir.CloseStream{
					Fd:    ir.String(redirection.Src),
					Label: ctx.label,
				})
			} else {
				buf.add(ir.DuplicateStream{Old: redirection.Src, New: g.handleExpression(buf, redirection.Dst)})
				if redirection.Close {
					buf.add(ir.CloseStream{Fd: g.handleExpression(buf, redirection.Dst), Label: ctx.label})
				}
			}
		case "<":
			buf.add(ir.OpenStream{
				Name:   fmt.Sprintf("stream%d", i),
				Target: g.handleExpression(buf, redirection.Dst),
				Mode:   ir.FLAG_READ,
				Label:  ctx.label,
			})
			buf.add(ir.AddStream{Fd: redirection.Src, StreamName: fmt.Sprintf("stream%d", i)})
		case "<<<":
			buf.add(ir.NewPipeBuffer{
				Value: ir.Concat{
					g.handleExpression(buf, redirection.Dst),
					ir.String("\n"),
				},
				Name:  fmt.Sprintf("buffer%d", i),
				Label: ctx.label,
			})
			buf.add(ir.Declare{Name: fmt.Sprintf("stream%d", i), Value: ir.Literal(fmt.Sprintf("buffer%d", i))})
			buf.add(ir.AddStream{Fd: redirection.Src, StreamName: fmt.Sprintf("stream%d", i)})
		case "<>":
			buf.add(ir.OpenStream{
				Name:   fmt.Sprintf("stream%d", i),
				Target: g.handleExpression(buf, redirection.Dst),
				Mode:   ir.FLAG_RW,
				Label:  ctx.label,
			})
			buf.add(ir.AddStream{Fd: redirection.Src, StreamName: fmt.Sprintf("stream%d", i)})
		}
	}
}

func (g *generator) handleParameterAssignment(buf *InstructionBuffer, p ast.ParameterAssignement) {
	buf.add(ir.Set{Name: "shell.ExitCode", Value: ir.Literal("0")})
	for _, assignment := range p {
		ins := ir.SetVar{
			Key:   assignment.Name,
			Value: ir.String(""),
		}
		if assignment.Value != nil {
			ins.Value = g.handleExpression(buf, assignment.Value)
		}

		buf.add(ins)
	}
}

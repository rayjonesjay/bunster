package runtime

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

func NewShell() *Shell {
	shell := &Shell{}

	shell.vars = newRepository[string]()
	shell.env = newRepository[string]()
	shell.localVars = newRepository[string]()
	shell.exportedVars = newRepository[struct{}]()
	shell.functions = newRepository[PredefinedCommand]()

	for _, env := range os.Environ() {
		envs := strings.SplitN(env, "=", 2)
		shell.env.set(envs[0], envs[1])
	}

	return shell
}

type ExitError int

func (e ExitError) Error() string {
	return fmt.Sprintf("exit code %d", e)
}

type PredefinedCommand func(shell *Shell, stdin, stdout, stderr Stream)

type Shell struct {
	parent    *Shell
	PID       int
	Path      string
	ExitCode  int
	Args      []string
	WaitGroup sync.WaitGroup
	Embed     fs.FS

	vars         *repository[string]
	env          *repository[string]
	localVars    *repository[string]
	exportedVars *repository[struct{}]
	functions    *repository[PredefinedCommand]
	defered      []func(*Shell, *StreamManager)
}

func (shell *Shell) Shift(n int) {
	if n <= len(shell.parent.Args) {
		shell.parent.Args = shell.parent.Args[n:]
	} else {
		shell.parent.Args = nil
	}
}

func (shell *Shell) ReadVar(name string) string {
	if value, ok := shell.getLocalVar(name); ok {
		return value
	}
	if value, ok := shell.vars.get(name); ok {
		return value
	}
	if value, ok := shell.env.get(name); ok {
		return value
	}
	if shell.parent != nil {
		return shell.parent.ReadVar(name)
	}
	return ""
}

func (shell *Shell) VarIsSet(name string) bool {
	if _, ok := shell.getLocalVar(name); ok {
		return true
	}
	if _, ok := shell.vars.get(name); ok {
		return true
	}
	if _, ok := shell.env.get(name); ok {
		return true
	}
	return false
}

func (shell *Shell) setLocalVar(name, value string) bool {
	if _, ok := shell.localVars.get(name); ok {
		shell.localVars.set(name, value)
		return true
	}
	if shell.parent != nil {
		return shell.parent.setLocalVar(name, value)
	}
	return false
}

func (shell *Shell) getLocalVar(name string) (string, bool) {
	if value, ok := shell.localVars.get(name); ok {
		return value, true
	}
	if shell.parent != nil {
		return shell.parent.getLocalVar(name)
	}
	return "", false
}

func (shell *Shell) SetVar(name string, value string) {
	if !shell.setLocalVar(name, value) {
		shell.vars.set(name, value)
	}
}

func (shell *Shell) SetLocalVar(name string, value string) {
	shell.localVars.set(name, value)
}

func (shell *Shell) SetExportVar(name string, value string) {
	shell.exportedVars.set(name, struct{}{})
	shell.vars.set(name, value)
}

func (shell *Shell) MarkVarAsExported(name string) {
	shell.exportedVars.set(name, struct{}{})
}

func (shell *Shell) ReadSpecialVar(name string) string {
	switch name {
	case "0":
		return shell.Path
	case "$":
		return strconv.FormatInt(int64(shell.PID), 10)
	case "#":
		return strconv.FormatInt(int64(len(shell.Args)), 10)
	case "?":
		return strconv.FormatInt(int64(shell.ExitCode), 10)
	case "*", "@":
		return strings.Join(shell.Args, " ")
	default:
		index, err := strconv.ParseUint(name, 10, 64)
		if err != nil {
			return ""
		}
		if index <= uint64(len(shell.Args)) {
			return shell.Args[index-1]
		}
		return ""
	}
}

func (shell *Shell) HandleError(sm *StreamManager, err error) {
	shell.ExitCode = 1

	stderr, _err := sm.Get("2")
	if _err != nil {
		return
	}

	switch e := err.(type) {
	case *exec.Error:
		fmt.Fprintf(stderr, "%q: %v\n", e.Name, e.Err)
	case *fs.PathError:
		fmt.Fprintf(stderr, "%q: %v\n", e.Path, e.Err)
	case *exec.ExitError:
		shell.ExitCode = e.ExitCode()
	case ExitError:
		shell.ExitCode = int(e)
	default:
		fmt.Fprintln(stderr, err)
	}
}

func (shell *Shell) Clone() *Shell {
	sh := &Shell{
		PID:          shell.PID,
		Path:         shell.Path,
		ExitCode:     shell.ExitCode,
		Args:         shell.Args,
		Embed:        shell.Embed,
		functions:    shell.functions.clone(),
		vars:         shell.vars.clone(),
		localVars:    shell.localVars.clone(),
		env:          shell.env.clone(),
		exportedVars: shell.exportedVars.clone(),
	}

	return sh
}

func (shell *Shell) RegisterFunction(name string, handler PredefinedCommand) {
	shell.functions.set(name, handler)
}

func (shell *Shell) Defer(handler func(*Shell, *StreamManager)) {
	shell.defered = append(shell.defered, handler)
}

func (shell *Shell) Terminate(streamManager *StreamManager) {
	// defered commands run in LIFO order
	for i := len(shell.defered) - 1; i >= 0; i-- {
		shell.defered[i](shell, streamManager)
	}
}

func (shell *Shell) Command(name string, args ...string) *Command {
	var command Command
	command.shell = shell
	command.Args = args
	command.Name = name
	command.Env = make(map[string]string)

	if fn, ok := shell.functions.get(name); ok {
		command.function = fn
		return &command
	}

	return &command
}

type Command struct {
	shell  *Shell
	Name   string
	Args   []string
	Stdin  Stream
	Stdout Stream
	Stderr Stream
	Env    map[string]string

	ExitCode int

	function PredefinedCommand
	execCmd  *exec.Cmd
	wg       sync.WaitGroup
}

func (cmd *Command) Run() error {
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		return err
	}
	if cmd.function == nil {
		cmd.ExitCode = cmd.execCmd.ProcessState.ExitCode()
	}
	return nil
}

func (cmd *Command) Start() error {
	if cmd.function != nil {
		shell := Shell{
			parent:       cmd.shell,
			Path:         cmd.shell.Path,
			PID:          cmd.shell.PID,
			Embed:        cmd.shell.Embed,
			Args:         cmd.Args[:],
			functions:    cmd.shell.functions,
			vars:         cmd.shell.vars,
			env:          cmd.shell.env.clone(),
			localVars:    newRepository[string](),
			ExitCode:     cmd.shell.ExitCode,
			exportedVars: cmd.shell.exportedVars,
		}

		for key, value := range cmd.Env {
			shell.env.set(key, value)
		}

		cmd.wg.Add(1)
		go func() {
			cmd.function(&shell, cmd.Stdin, cmd.Stdout, cmd.Stderr)
			cmd.ExitCode = shell.ExitCode
			cmd.wg.Done()
		}()
		return nil
	}

	cmd.execCmd = exec.Command(cmd.Name, cmd.Args...) //nolint:gosec
	cmd.execCmd.Stdin = cmd.Stdin
	cmd.execCmd.Stdout = cmd.Stdout
	cmd.execCmd.Stderr = cmd.Stderr

	cmd.shell.env.foreach(func(key string, value string) bool {
		cmd.execCmd.Env = append(cmd.execCmd.Env, fmt.Sprintf("%s=%s", key, value))
		return true
	})
	cmd.shell.exportedVars.foreach(func(key string, _ struct{}) bool {
		cmd.execCmd.Env = append(cmd.execCmd.Env, fmt.Sprintf("%s=%s", key, cmd.shell.ReadVar(key)))
		return true
	})
	for key, value := range cmd.Env {
		cmd.execCmd.Env = append(cmd.execCmd.Env, key+"="+value)
	}

	return cmd.execCmd.Start()
}

func (cmd *Command) Wait() error {
	if cmd.function != nil {
		cmd.wg.Wait()
		if cmd.ExitCode != 0 {
			return ExitError(cmd.ExitCode)
		}
		return nil
	}
	return cmd.execCmd.Wait()
}

type repository[T any] struct {
	mx   sync.RWMutex
	data map[string]T
}

func newRepository[T any]() *repository[T] {
	return &repository[T]{
		data: make(map[string]T),
	}
}

func (r *repository[T]) get(key string) (T, bool) {
	r.mx.RLock()
	defer r.mx.RUnlock()
	v, ok := r.data[key]
	return v, ok
}

func (r *repository[T]) set(key string, value T) {
	r.mx.Lock()
	defer r.mx.Unlock()
	r.data[key] = value
}

func (r *repository[T]) clone() *repository[T] {
	var repo = newRepository[T]()
	for key, value := range r.data {
		repo.set(key, value)
	}
	return repo
}

func (r *repository[T]) foreach(fn func(key string, value T) bool) {
	r.mx.RLock()
	defer r.mx.RUnlock()
	for key, value := range r.data {
		if !fn(key, value) {
			break
		}
	}
}

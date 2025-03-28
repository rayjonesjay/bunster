package builtin

import (
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/yassinebenaid/bunster/runtime"
)

func Embed(shell *runtime.Shell, stdin, stdout, stderr runtime.Stream) {
	if shell.Embed == nil {
		fmt.Fprintf(stderr, "embed: no files were embedded\n")
		shell.ExitCode = 1
		return
	}

	if len(shell.Args) != 2 {
		fmt.Fprintf(stderr, "embed: expected 2 arguments, got %d\n", len(shell.Args))
		shell.ExitCode = 1
		return
	}

	command, path := shell.Args[0], shell.Args[1]

	switch command {
	case "cat":
		f, err := shell.Embed.Open(path)
		if err != nil {
			fmt.Fprintf(stderr, "embed: %v\n", err)
			shell.ExitCode = 1
			return
		}
		if _, err := io.Copy(stdout, f); err != nil {
			fmt.Fprintf(stderr, "embed: %v\n", err)
			shell.ExitCode = 1
			return
		}
	case "ls":
		de, err := fs.ReadDir(shell.Embed, path)
		if err != nil {
			fmt.Fprintf(stderr, "embed: %v\n", err)
			shell.ExitCode = 1
			return
		}

		var files []string
		for _, entry := range de {
			files = append(files, entry.Name())
		}

		fmt.Fprintln(stdout, strings.Join(files, "\n"))
	default:
		fmt.Fprintf(stderr, "embed: %q is not a valid embed command\n", command)
		shell.ExitCode = 1
	}
}

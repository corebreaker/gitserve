package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
)

const defaultPort = 8085

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command line args, given without the program name, and returns the exit status:
// 0 on success, 2 for a usage error, and 1 when the command fails.
func run(args []string, stdout, stderr io.Writer) int {
	root := newRootCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	// Cobra parses the flags and checks the arguments before running a command:
	// an error returned before then is a usage error.
	started := false
	root.PersistentPreRun = func(*cobra.Command, []string) { started = true }

	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}

	for line := range strings.SplitSeq(err.Error(), "\n") {
		fmt.Fprintf(stderr, "gitserve: %s\n", line)
	}

	if started {
		return 1
	}

	fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
	return 2
}

// newRootCommand returns the command line interface of gitserve.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "gitserve",
		Short: "Serve local bare Git repositories over HTTP",
		Long: `gitserve serves bare Git repositories over the smart HTTP protocol of Git,
so that they can be cloned and pushed to without a forge, and without Git installed on the serving machine.

The repositories live in a store: a directory, or a zip archive when its name ends with .zip.
An archive is loaded in memory, and written back to its file after each push.`,
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}

	root.AddCommand(newServeCommand(), newInitCommand(), newCreateCommand(), newDeleteCommand(), newListCommand())
	return root
}

// newServeCommand returns the command that serves the repositories of a store.
func newServeCommand() *cobra.Command {
	var port int
	var prefix string

	cmd := &cobra.Command{
		Use:   "serve [flags] <store>",
		Short: "Serve the repositories of a store over HTTP",
		Long: `Serve the repositories of a store over HTTP on all network interfaces, until interrupted.
A store that does not exist yet is initialized first.

The repository in the directory <name> of the store is served at http://<host>:<port><prefix><name>.
When <name> ends with .git, the repository is also served without that suffix.`,
		Example: "  gitserve serve gitroot\n  gitserve serve --port 9000 --prefix /git repos.zip",
		Args: func(cmd *cobra.Command, args []string) error {
			if port < 0 || port > 65535 {
				return fmt.Errorf("invalid port %d", port)
			}

			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			stop := make(chan os.Signal, 1)
			signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(stop)

			return runServer(args[0], net.JoinHostPort("", strconv.Itoa(port)), normalizePrefix(prefix), stop)
		},
	}

	cmd.Flags().IntVarP(&port, "port", "p", defaultPort, "port to listen on")
	cmd.Flags().StringVar(&prefix, "prefix", "/", "URL path under which the repositories are served")
	return cmd
}

// runServer serves the repositories of the store at path on the TCP address addr, under the URL prefix,
// until a signal arrives on stop.
func runServer(path, addr, prefix string, stop <-chan os.Signal) error {
	created, err := ensureStore(path)
	if err != nil {
		return err
	}

	if created {
		log.Printf("Initialized empty store %s", path)
	}

	st, err := openStore(path)
	if err != nil {
		return err
	}

	// Once the server stopped, let a push being saved finish before the process exits.
	defer st.close()

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	log.Printf("Serving %s on port %d, under %s", path, listener.Addr().(*net.TCPAddr).Port, prefix)

	server := &http.Server{Handler: &requestLogger{next: &gitHandler{store: st, prefix: prefix}}}
	return serve(server, listener, stop)
}

// normalizePrefix returns the URL prefix with a leading and a trailing slash.
func normalizePrefix(prefix string) string {
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}

	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	return prefix
}

// newInitCommand returns the command that creates an empty store.
func newInitCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init <store>",
		Short: "Create an empty store",
		Long: `Create an empty store, along with its missing parent directories:
a directory, or a zip archive when the name ends with .zip.

An archive holds the repositories at its root, as a directory does,
next to a .gitserve file that records the version of the archive format.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := initStore(args[0]); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Initialized empty store %s\n", args[0])
			return nil
		},
	}
}

// newCreateCommand returns the command that creates repositories.
func newCreateCommand() *cobra.Command {
	var branch string

	cmd := &cobra.Command{
		Use:   "create [flags] <store> <repository>...",
		Short: "Create bare repositories",
		Long: `Create empty bare repositories in a store. A store that does not exist yet is initialized first.
A repository name may contain slashes, to create the repository in a subdirectory of the store.`,
		Example: "  gitserve create gitroot my-project.git\n  gitserve create --branch master repos.zip team/tools",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := plumbing.NewBranchReferenceName(branch).Validate(); err != nil {
				return fmt.Errorf("invalid branch name %q", branch)
			}

			return cobra.MinimumNArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			created, err := ensureStore(args[0])
			if err != nil {
				return err
			}

			if created {
				fmt.Fprintf(out, "Initialized empty store %s\n", args[0])
			}

			st, err := openStore(args[0])
			if err != nil {
				return err
			}

			names, err := st.createRepositories(args[1:], branch)
			for _, name := range names {
				fmt.Fprintf(out, "Created repository %s\n", name)
			}

			return err
		},
	}

	cmd.Flags().StringVarP(&branch, "branch", "b", "main", "branch that HEAD points to in the new repositories")
	return cmd
}

// newDeleteCommand returns the command that deletes repositories.
func newDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <store> <repository>...",
		Short: "Delete repositories",
		Long: `Delete repositories from a store, with all their content.
As when serving, a name also designates the repository that bears it followed by .git.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore(args[0])
			if err != nil {
				return err
			}

			dirs, err := st.deleteRepositories(args[1:])
			for _, dir := range dirs {
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted repository %s\n", dir)
			}

			return err
		},
	}
}

// newListCommand returns the command that lists the repositories of a store.
func newListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list <store>",
		Short: "List the repositories of a store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore(args[0])
			if err != nil {
				return err
			}

			names, err := st.repositories()
			for _, name := range names {
				fmt.Fprintln(cmd.OutOrStdout(), name)
			}

			return err
		},
	}
}

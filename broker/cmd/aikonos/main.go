// aikonos — developer scaffolding CLI.
// Usage: aikonos new <tool|connector|policy> <name> [--root <dir>]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/adamdekan/aikonos/broker/cmd/aikonos/scaffold"
)

const usage = `Usage: aikonos new <tool|connector|policy|migration|view> <name> [--root <dir>]

Subcommands:
  tool       Generate skills/<name>/skill.yaml + skills/<name>/main.py
  connector  Generate broker/internal/connector/<name>_plugin.go (Plugin stub)
  policy     Generate policies/opa/<name>.rego (pluggable gate skeleton)
  migration  Generate the next-numbered broker/internal/db/migrations/NNN_<name>.sql
  view       Generate webui/web/src/views/admin/<Name>.vue (admin view skeleton)

Flags:
  --root  Repo root directory (default ".")
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 1 || args[0] != "new" {
		return fmt.Errorf("unknown command %q", firstOrEmpty(args))
	}

	fs := flag.NewFlagSet("aikonos new", flag.ContinueOnError)
	root := fs.String("root", ".", "repo root directory")
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	// args[1:] = <subcommand> <name> [--root ...]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("aikonos new requires <tool|connector|policy|migration|view> <name>")
	}

	sub, name := rest[0], rest[1]

	switch sub {
	case "tool":
		path, err := scaffold.NewTool(*root, name)
		if err != nil {
			return err
		}
		fmt.Printf("created %s\n", path)
		fmt.Printf("created %s\n", path[:len(path)-len("skill.yaml")]+"main.py")
		fmt.Println("Next: the broker loads skills/<name>/skill.yaml on startup — no recompile needed.")

	case "policy":
		path, err := scaffold.NewPolicy(*root, name)
		if err != nil {
			return err
		}
		fmt.Printf("created %s\n", path)
		fmt.Printf("Next: call RegisterToolGate(%q, \"aikonos/%s\") in broker/cmd/broker/main.go, then apply the updated opa-policies ConfigMap.\n", name, name)

	case "connector":
		path, err := scaffold.NewConnector(*root, name)
		if err != nil {
			return err
		}
		fmt.Printf("created %s\n", path)
		fmt.Println("Next: add an AppCredentials slot on Registry + creds; fill the TODOs in the generated file.")

	case "migration":
		path, err := scaffold.NewMigration(name, *root)
		if err != nil {
			return err
		}
		fmt.Printf("created %s\n", path)
		fmt.Println("Next: apply the migration with deploy/compose/migrate.sh.")

	case "view":
		// NewAdminView prints next-steps wiring instructions itself.
		if _, err := scaffold.NewAdminView(name, *root); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown subcommand %q (want: tool, connector, policy, migration, view)", sub)
	}

	return nil
}

func firstOrEmpty(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

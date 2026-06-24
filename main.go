package main

import (
	"embed"
	"flag"
	"fmt"
	"os"

	"github.com/algonous/agent-sessions/internal/data"
)

//go:embed web
var webFS embed.FS

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "view":
			runView(args[1:])
			return
		case "open":
			runOpen(args[1:])
			return
		case "stop":
			runStop(args[1:])
			return
		case "info":
			runInfo(args[1:])
			return
		case "serve":
			runServe(args[1:])
			return
		case "migrate":
			runMigrate(args[1:])
			return
		case "help", "-h", "--help":
			printUsage()
			return
		}
	}
	runView(args)
}

func runMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	source := fs.String("source", "auto", "session source: auto, claude, or codex")
	dataDir := fs.String("data-dir", "", "source data root; defaults to ~/.claude or ~/.codex")
	dryRun := fs.Bool("dry-run", false, "show migration plan without changing files")
	backupDir := fs.String("backup-dir", "", "directory for backups; defaults under the source data root")
	fs.Parse(args)

	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: agent-sessions migrate [options] <session-id> <target-dir>")
		fs.PrintDefaults()
		os.Exit(2)
	}

	plan, err := data.MigrateSession(data.MigrationOptions{
		Source:    *source,
		DataDir:   *dataDir,
		SessionID: fs.Arg(0),
		TargetDir: fs.Arg(1),
		DryRun:    *dryRun,
		BackupDir: *backupDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printMigrationPlan(plan, *dryRun)
}

func printUsage() {
	fmt.Println(`agent-sessions browses and migrates Claude Code and Codex sessions.

Usage:
  agent-sessions [view options]
  agent-sessions view [view options]
  agent-sessions open [view options]
  agent-sessions stop
  agent-sessions info
  agent-sessions migrate [migrate options] <session-id> <target-dir>

Run "agent-sessions view -h" or "agent-sessions migrate -h" for command options.`)
}

func printMigrationPlan(plan *data.MigrationPlan, dryRun bool) {
	if dryRun {
		fmt.Println("dry-run: no changes made")
	} else {
		fmt.Println("migration complete")
	}
	fmt.Printf("source: %s\n", plan.Source)
	fmt.Printf("session: %s\n", plan.SessionID)
	fmt.Printf("from: %s\n", plan.OldProject)
	fmt.Printf("to: %s\n", plan.TargetDir)
	if plan.BackupDir != "" {
		fmt.Printf("backup: %s\n", plan.BackupDir)
	}
	if len(plan.Warnings) > 0 {
		fmt.Println("warnings:")
		for _, warning := range plan.Warnings {
			fmt.Printf("- %s\n", warning)
		}
	}
	if len(plan.Operations) > 0 {
		fmt.Println("operations:")
		for _, op := range plan.Operations {
			if op.Detail == "" {
				fmt.Printf("- %s %s\n", op.Action, op.Path)
			} else {
				fmt.Printf("- %s %s: %s\n", op.Action, op.Path, op.Detail)
			}
		}
	}
}

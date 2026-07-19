package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

func runBrief(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("brief", flag.ContinueOnError)
	repo := fs.String("repo", ".", "仓库路径")
	budget := fs.Int("budget", 1200, "简报 estimated-token 上限(300..4000)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := store.Open(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	brief, err := engine.New(s).Brief(*budget)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	if _, err := fmt.Fprintln(out, brief); err != nil {
		fmt.Fprintln(os.Stderr, "错误:写简报输出:", err)
		return 1
	}
	return 0
}

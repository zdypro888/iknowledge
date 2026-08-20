package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type iknowledgeProcess struct {
	PID     int
	PPID    int
	Elapsed string
	RSSKiB  int64
	Kind    string
	Command string
}

func deployProcessText() (string, int) {
	// ucomm is the kernel process name, kept separate from args so a shell,
	// editor, or test whose arguments merely mention "iknowledge serve" cannot
	// be mistaken for the actual binary.
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,etime=,rss=,ucomm=,args=").Output()
	if err != nil { // Windows/精简容器没有 ps；PATH/binary 检查仍可继续。
		return "", 0
	}
	processes := parseIKnowledgeProcesses(string(out))
	if len(processes) == 0 {
		return "  processes: no active iknowledge stdio/serve\n", 0
	}
	var bridges, daemons int
	var bridgeRSS, daemonRSS int64
	oldestBridge := ""
	var daemonLines []string
	warnings := 0
	for _, process := range processes {
		switch process.Kind {
		case "stdio":
			bridges++
			bridgeRSS += process.RSSKiB
			if elapsedGreater(process.Elapsed, oldestBridge) {
				oldestBridge = process.Elapsed
			}
		case "serve":
			daemons++
			daemonRSS += process.RSSKiB
			daemonLines = append(daemonLines, fmt.Sprintf("    serve pid=%d age=%s rss=%.1fMiB %s",
				process.PID, process.Elapsed, float64(process.RSSKiB)/1024, process.Command))
			if process.RSSKiB > 512<<10 {
				warnings++
				daemonLines = append(daemonLines, "      ⚠ resident RSS 超过 512MiB，建议用真实仓库做 heap/recall profile")
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  processes: stdio=%d rss=%.1fMiB", bridges, float64(bridgeRSS)/1024)
	if oldestBridge != "" {
		fmt.Fprintf(&b, " oldest=%s", oldestBridge)
	}
	fmt.Fprintf(&b, " | serve=%d rss=%.1fMiB\n", daemons, float64(daemonRSS)/1024)
	if bridges > 8 {
		warnings++
		b.WriteString("  ⚠ stdio bridge 数量异常偏高；检查是否把 repo 专属 MCP 放进了全局 Codex 配置\n")
	}
	for _, line := range daemonLines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), warnings
}

func parseIKnowledgeProcesses(output string) []iknowledgeProcess {
	var out []iknowledgeProcess
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		kind := iknowledgeProcessKind(fields[4], fields[5:])
		if kind == "" {
			continue
		}
		command := strings.Join(fields[5:], " ")
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		rss, rssErr := strconv.ParseInt(fields[3], 10, 64)
		if pidErr != nil || ppidErr != nil || rssErr != nil || rss < 0 {
			continue
		}
		out = append(out, iknowledgeProcess{
			PID: pid, PPID: ppid, Elapsed: fields[2], RSSKiB: rss, Kind: kind, Command: command,
		})
	}
	return out
}

func iknowledgeProcessKind(executable string, argv []string) string {
	if executable != "iknowledge" || len(argv) < 2 {
		return ""
	}
	// argv[0] is the invoked binary path/name. iknowledge has no global flags,
	// so stdio/serve are valid only as the immediate subcommand in argv[1].
	switch argv[1] {
	case "stdio", "serve":
		return argv[1]
	default:
		return ""
	}
}

func elapsedGreater(a, b string) bool {
	return elapsedSeconds(a) > elapsedSeconds(b)
}

func elapsedSeconds(value string) int64 {
	var days int64
	clock := value
	if before, after, ok := strings.Cut(value, "-"); ok {
		parsed, err := strconv.ParseInt(before, 10, 64)
		if err != nil {
			return 0
		}
		days, clock = parsed, after
	}
	parts := strings.Split(clock, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	var nums []int64
	for _, part := range parts {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || n < 0 {
			return 0
		}
		nums = append(nums, n)
	}
	seconds := days * 24 * 60 * 60
	if len(nums) == 2 {
		return seconds + nums[0]*60 + nums[1]
	}
	return seconds + nums[0]*60*60 + nums[1]*60 + nums[2]
}

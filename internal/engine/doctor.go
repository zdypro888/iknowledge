package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/parser"
)

// ParserHealthReport 汇总当前仓库各解析器覆盖与失败样本。
type ParserHealthReport struct {
	Files    int
	Symbols  int
	ByLang   map[string]ParserLangHealth
	Failures []ParserFailure
}

type ParserLangHealth struct {
	Files   int
	Symbols int
}

type ParserFailure struct {
	File string
	Lang string
	Err  string
}

// ParserHealth 扫描当前受配置约束的源文件,给出 parser 可用性仪表盘。
func (e *Engine) ParserHealth() (ParserHealthReport, error) {
	var rep ParserHealthReport
	rep.ByLang = map[string]ParserLangHealth{}
	files, err := e.cachedSourceFiles()
	if err != nil {
		return rep, err
	}
	for _, rel := range files {
		p := e.Reg.ForFile(rel)
		if p == nil {
			continue
		}
		src, err := safeRepoRead(e.Store.RepoRoot(), rel)
		if err != nil || parser.IsGenerated(src) {
			continue
		}
		rep.Files++
		lang := p.Language()
		syms, perr := p.Parse(rel, src)
		if perr != nil {
			rep.Failures = append(rep.Failures, ParserFailure{File: rel, Lang: lang, Err: perr.Error()})
			continue
		}
		rep.Symbols += len(syms)
		h := rep.ByLang[lang]
		h.Files++
		h.Symbols += len(syms)
		rep.ByLang[lang] = h
	}
	sort.Slice(rep.Failures, func(i, j int) bool { return rep.Failures[i].File < rep.Failures[j].File })
	return rep, nil
}

func (r ParserHealthReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "parser: files=%d symbols=%d failures=%d", r.Files, r.Symbols, len(r.Failures))
	var langs []string
	for lang := range r.ByLang {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	for _, lang := range langs {
		h := r.ByLang[lang]
		fmt.Fprintf(&b, "\n  - %s: files=%d symbols=%d", lang, h.Files, h.Symbols)
	}
	for i, f := range r.Failures {
		if i >= 10 {
			fmt.Fprintf(&b, "\n  ... 另有 %d 个解析失败文件", len(r.Failures)-i)
			break
		}
		fmt.Fprintf(&b, "\n  ! %s(%s): %s", f.File, f.Lang, f.Err)
	}
	return b.String()
}

// DoctorReport 是仓库级自检:用于 CLI doctor,也给未来 MCP 暴露留出口。
type DoctorReport struct {
	RepoRoot    string
	Initialized bool
	ConfigPort  int
	GitFilesOK  bool
	Debts       int
	WIPs        int
	Parser      ParserHealthReport
	Warnings    []string
}

func (r DoctorReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "doctor: %s\n", r.RepoRoot)
	if r.Initialized {
		fmt.Fprintf(&b, "init: ok")
		if r.ConfigPort > 0 {
			fmt.Fprintf(&b, " port=%d", r.ConfigPort)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("init: missing(.knowledge/project.yaml 不存在)\n")
	}
	if r.GitFilesOK {
		b.WriteString("git merge rules: ok\n")
	} else {
		b.WriteString("git merge rules: missing(.gitattributes/.gitignore 需重跑 init)\n")
	}
	b.WriteString(r.Parser.Text())
	fmt.Fprintf(&b, "\nmaintain: debts=%d active_wip=%d", r.Debts, r.WIPs)
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "\n⚠ %s", w)
	}
	return b.String()
}

// Doctor 做只读仓库自检。未初始化时只报告基础状态。
func (e *Engine) Doctor() (DoctorReport, error) {
	rep := DoctorReport{RepoRoot: e.Store.RepoRoot(), Initialized: e.Store.Initialized(), GitFilesOK: e.Store.GitFilesOK()}
	if rep.Initialized {
		if err := e.Sync(); err != nil {
			return rep, err
		}
	}
	if cfg, err := e.Store.LoadConfig(); err != nil {
		rep.Warnings = append(rep.Warnings, "config.yaml 解析失败:"+err.Error())
	} else if cfg != nil {
		rep.ConfigPort = cfg.Port
	}
	if !rep.Initialized {
		rep.Warnings = append(rep.Warnings, "先运行 iknowledge init --repo "+e.Store.RepoRoot())
		return rep, nil
	}
	ph, err := e.ParserHealth()
	if err != nil {
		rep.Warnings = append(rep.Warnings, "parser health 扫描失败:"+err.Error())
	} else {
		rep.Parser = ph
	}
	e.rt.mu.RLock()
	rep.Debts = len(e.computeDebtsLocked())
	rep.WIPs = len(e.rt.wips)
	e.rt.mu.RUnlock()
	return rep, nil
}

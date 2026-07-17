package engine

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// RedactionReport 描述一次写入在落盘前移除的秘密。Kinds 只含命中的类型,
// 不含秘密原文,因此可以安全地返回给调用方和写入运行日志。
type RedactionReport struct {
	Count int
	Kinds []string
}

type secretPattern struct {
	kind        string
	re          *regexp.Regexp
	replacement string
}

// 模式刻意偏向高置信凭证,避免把普通知识文本当秘密吞掉。credential-assignment
// 只接收至少 12 字符且不含空白的值,覆盖尚无稳定厂商前缀的常见配置写法。
var secretPatterns = []secretPattern{
	{kind: "private-key", re: regexp.MustCompile(`(?s)-----BEGIN(?: [A-Z0-9]+)? PRIVATE KEY-----.*?-----END(?: [A-Z0-9]+)? PRIVATE KEY-----`), replacement: `[REDACTED:private-key]`},
	{kind: "url-password", re: regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/\s:@]+:)[^@\s/]+@`), replacement: `${1}[REDACTED:url-password]@`},
	{kind: "anthropic-key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`), replacement: `[REDACTED:anthropic-key]`},
	{kind: "openai-key", re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), replacement: `[REDACTED:openai-key]`},
	{kind: "github-token", re: regexp.MustCompile(`\b(?:github_pat_[A-Za-z0-9_]{20,}|gh[pousr]_[A-Za-z0-9]{20,})\b`), replacement: `[REDACTED:github-token]`},
	{kind: "aws-access-key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), replacement: `[REDACTED:aws-access-key]`},
	{kind: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}\b`), replacement: `[REDACTED:google-api-key]`},
	{kind: "slack-token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), replacement: `[REDACTED:slack-token]`},
	{kind: "stripe-key", re: regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b`), replacement: `[REDACTED:stripe-key]`},
	{kind: "bearer-token", re: regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/-]{20,}={0,2}`), replacement: `Bearer [REDACTED:bearer-token]`},
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), replacement: `[REDACTED:jwt]`},
	// credential 分三种语法分别保留原分隔符/引号。不能用一个可选引号模式:
	// `password: "value"` 若吞掉开引号却留下闭引号,会把合法 YAML 脱敏成坏文件。
	{kind: "credential", re: regexp.MustCompile(`(?i)("(?:api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|password|passwd)"\s*:\s*")[^"\\\s]{12,}(")`), replacement: `${1}[REDACTED:credential]${2}`},
	{kind: "credential", re: regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|password|passwd)\b(\s*[:=]\s*)(["'])[A-Za-z0-9_./+~=-]{12,}(["'])`), replacement: `${1}${2}${3}[REDACTED:credential]${4}`},
	{kind: "credential", re: regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|password|passwd)\b(\s*[:=]\s*)[A-Za-z0-9_./+~=-]{12,}`), replacement: `${1}${2}[REDACTED:credential]`},
}

// RedactText 清理一段任意文本。它用于 bundle 导入这类无法依赖 Go 结构标签的入口。
func RedactText(s string) (string, RedactionReport) {
	kinds := map[string]bool{}
	count := 0
	for _, p := range secretPatterns {
		n := len(p.re.FindAllStringIndex(s, -1))
		if n == 0 {
			continue
		}
		s = p.re.ReplaceAllString(s, p.replacement)
		count += n
		kinds[p.kind] = true
	}
	outKinds := make([]string, 0, len(kinds))
	for kind := range kinds {
		outKinds = append(outKinds, kind)
	}
	sort.Strings(outKinds)
	return s, RedactionReport{Count: count, Kinds: outKinds}
}

// RedactSecrets 只遍历显式标有 redact:"true" 的字段。这样安全策略不会误改
// node ID、源码路径、author 等结构性标识;嵌套结构仍会继续寻找自己的标签。
func RedactSecrets(v any) RedactionReport {
	report := RedactionReport{}
	kinds := map[string]bool{}
	redactValue(reflect.ValueOf(v), false, &report, kinds)
	for kind := range kinds {
		report.Kinds = append(report.Kinds, kind)
	}
	sort.Strings(report.Kinds)
	return report
}

func redactValue(v reflect.Value, force bool, report *RedactionReport, kinds map[string]bool) {
	if !v.IsValid() {
		return
	}
	if v.Kind() == reflect.Pointer {
		if !v.IsNil() {
			redactValue(v.Elem(), force, report, kinds)
		}
		return
	}
	switch v.Kind() {
	case reflect.String:
		if !force || !v.CanSet() {
			return
		}
		clean, r := RedactText(v.String())
		if r.Count > 0 {
			v.SetString(clean)
			report.Count += r.Count
			for _, kind := range r.Kinds {
				kinds[kind] = true
			}
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if !field.CanSet() {
				continue
			}
			redactValue(field, force || t.Field(i).Tag.Get("redact") == "true", report, kinds)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			redactValue(v.Index(i), force, report, kinds)
		}
	case reflect.Map:
		// 目前受保护模型没有自由文本 map。reflect map 值不可直接 Set;若未来增加,
		// 应改成有类型字段并加标签,避免无差别改写结构性映射。
		return
	}
}

func withRedactionNotice(out string, report RedactionReport) string {
	if report.Count == 0 {
		return out
	}
	notice := fmt.Sprintf("⚠ 安全:写入前已脱敏 %d 处秘密(%s);原文未落盘。",
		report.Count, strings.Join(report.Kinds, ","))
	if strings.TrimSpace(out) == "" {
		return notice
	}
	return out + "\n" + notice
}

func appendRedactionNotice(out *string, err *error, report RedactionReport) {
	if *err == nil {
		*out = withRedactionNotice(*out, report)
	}
}

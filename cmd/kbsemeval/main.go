// kbsemeval is a deterministic semantic-retrieval regression harness. It
// evaluates precomputed vectors and human qrels without contacting a model.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/vector"
)

const (
	evalSchema       = "iknowledge-semantic-qrels-v1"
	maxEvalLineBytes = 16 << 20
	maxEvalFileBytes = 64 << 20
	maxEvalCases     = 10_000
)

var evalLanes = []string{"current", "risk", "history"}

const usage = `kbsemeval——离线 semantic 检索算法回归基线

用法:
  kbsemeval --input <qrels.jsonl>
      [--min-recall 1] [--min-lane-precision 1]
      [--min-distinct-node-rate 1] [--max-ranking-violations 0]

退出码: 0=达到阈值，1=数据非法或未达阈值，2=命令参数非法。
本工具只评估预计算向量与人工 qrels；不会调用 embedding 服务。
`

type evalRecord struct {
	ID           string    `json:"id"`
	NodeID       string    `json:"node_id"`
	Lane         string    `json:"lane"`
	ExpectedLane string    `json:"expected_lane"`
	Vector       []float32 `json:"vector"`
}

type evalCase struct {
	Schema       string              `json:"schema"`
	ID           string              `json:"id"`
	TopK         int                 `json:"top_k"`
	QueryVector  []float32           `json:"query_vector"`
	Records      []evalRecord        `json:"records"`
	Qrels        map[string][]string `json:"qrels"`
	ExpectedHits map[string][]string `json:"expected_hits"`
}

type laneCounts struct {
	expected      int
	found         int
	reciprocalSum float64
	queries       int
}

type laneReport struct {
	Expected int     `json:"expected"`
	Found    int     `json:"found"`
	Recall   float64 `json:"recall_at_k"`
	MRR      float64 `json:"mrr"`
}

type evalReport struct {
	Schema            string                `json:"schema"`
	Cases             int                   `json:"cases"`
	Lanes             map[string]laneReport `json:"lanes"`
	Recall            float64               `json:"recall_at_k"`
	LanePrecision     float64               `json:"lane_precision"`
	DistinctNodeRate  float64               `json:"distinct_node_rate"`
	RankingViolations int                   `json:"ranking_violations"`
}

type thresholds struct {
	minRecall            float64
	minLanePrecision     float64
	minDistinctNodeRate  float64
	maxRankingViolations int
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("kbsemeval", flag.ContinueOnError)
	fs.SetOutput(errOut)
	input := fs.String("input", "", "版本化 JSONL qrels/向量 fixture")
	minRecall := fs.Float64("min-recall", 1, "每个有 qrels 的 lane 的最低 Recall@K")
	minLanePrecision := fs.Float64("min-lane-precision", 1, "最低通道精度")
	minDistinctRate := fs.Float64("min-distinct-node-rate", 1, "最低 lane 内节点去重率")
	maxRankingViolations := fs.Int("max-ranking-violations", 0, "最多允许的 Top-K 顺序/节点赢家偏差")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *input == "" || fs.NArg() != 0 || !unitInterval(*minRecall) ||
		!unitInterval(*minLanePrecision) || !unitInterval(*minDistinctRate) || *maxRankingViolations < 0 {
		fmt.Fprint(errOut, usage)
		return 2
	}

	cases, err := loadEvalCases(*input)
	if err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	report, err := evaluateCases(context.Background(), cases)
	if err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	printReport(out, report)
	gate := thresholds{
		minRecall: *minRecall, minLanePrecision: *minLanePrecision,
		minDistinctNodeRate: *minDistinctRate, maxRankingViolations: *maxRankingViolations,
	}
	if failures := reportFailures(report, gate); len(failures) > 0 {
		fmt.Fprintf(out, "FAIL: %s\n", strings.Join(failures, "; "))
		return 1
	}
	fmt.Fprintln(out, "PASS: semantic retrieval regression baseline")
	return 0
}

func unitInterval(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func loadEvalCases(path string) ([]evalCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取 qrels: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxEvalLineBytes)
	cases := make([]evalCase, 0, 32)
	lineNumber := 0
	totalBytes := 0
	for scanner.Scan() {
		lineNumber++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if len(line) > maxEvalFileBytes-totalBytes {
			return nil, fmt.Errorf("qrels 正文超过 %d MiB", maxEvalFileBytes>>20)
		}
		totalBytes += len(line)
		if len(cases) >= maxEvalCases {
			return nil, fmt.Errorf("qrels 超过 %d 个 case", maxEvalCases)
		}
		var item evalCase
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&item); err != nil {
			return nil, fmt.Errorf("qrels 第 %d 行: %w", lineNumber, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				err = fmt.Errorf("尾随 JSON")
			}
			return nil, fmt.Errorf("qrels 第 %d 行: %w", lineNumber, err)
		}
		cases = append(cases, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("扫描 qrels: %w", err)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("qrels 没有 case")
	}
	return cases, nil
}

func evaluateCases(ctx context.Context, cases []evalCase) (evalReport, error) {
	report := evalReport{Schema: evalSchema, Cases: len(cases), Lanes: make(map[string]laneReport, len(evalLanes))}
	counts := make(map[string]*laneCounts, len(evalLanes))
	for _, lane := range evalLanes {
		counts[lane] = &laneCounts{}
	}
	seenCase := make(map[string]bool, len(cases))
	correctLane, totalHits := 0, 0
	distinctHits := 0

	for caseIndex, item := range cases {
		if err := validateEvalCase(item, seenCase); err != nil {
			return evalReport{}, fmt.Errorf("case %d (%q): %w", caseIndex+1, item.ID, err)
		}
		seenCase[item.ID] = true
		records := make([]vector.Record, len(item.Records))
		byID := make(map[string]evalRecord, len(item.Records))
		for i, record := range item.Records {
			records[i] = vector.Record{ID: record.ID, NodeID: record.NodeID, Kind: record.Lane, Vector: record.Vector}
			byID[record.ID] = record
		}
		snapshot, err := vector.Build(len(item.QueryVector), records)
		if err != nil {
			return evalReport{}, fmt.Errorf("case %q 构建 Flat index: %w", item.ID, err)
		}
		byLane, err := snapshot.SearchDistinctNodesByKinds(ctx, item.QueryVector, item.TopK, evalLanes)
		if err != nil {
			return evalReport{}, fmt.Errorf("case %q 搜索: %w", item.ID, err)
		}

		for _, lane := range evalLanes {
			hits := byLane[lane]
			if len(hits) > item.TopK {
				return evalReport{}, fmt.Errorf("case %q lane=%s 返回 %d 条，超过 top_k=%d", item.ID, lane, len(hits), item.TopK)
			}
			totalHits += len(hits)
			seenNode := make(map[string]bool, len(hits))
			ranks := make(map[string]int, len(hits))
			gotIDs := make([]string, 0, len(hits))
			for rank, hit := range hits {
				gotIDs = append(gotIDs, hit.ID)
				record := byID[hit.ID]
				if record.ExpectedLane == lane {
					correctLane++
				}
				if !seenNode[hit.NodeID] {
					seenNode[hit.NodeID] = true
					distinctHits++
				}
				if _, exists := ranks[hit.NodeID]; !exists {
					ranks[hit.NodeID] = rank + 1
				}
			}
			if !slices.Equal(gotIDs, item.ExpectedHits[lane]) {
				report.RankingViolations++
			}

			relevant := item.Qrels[lane]
			if len(relevant) == 0 {
				continue
			}
			laneCount := counts[lane]
			laneCount.queries++
			laneCount.expected += len(relevant)
			firstRelevantRank := 0
			for _, nodeID := range relevant {
				if rank := ranks[nodeID]; rank > 0 {
					laneCount.found++
					if firstRelevantRank == 0 || rank < firstRelevantRank {
						firstRelevantRank = rank
					}
				}
			}
			if firstRelevantRank > 0 {
				laneCount.reciprocalSum += 1 / float64(firstRelevantRank)
			}
		}
	}

	totalExpected, totalFound := 0, 0
	for _, lane := range evalLanes {
		count := counts[lane]
		laneResult := laneReport{Expected: count.expected, Found: count.found}
		if count.expected > 0 {
			laneResult.Recall = float64(count.found) / float64(count.expected)
		}
		if count.queries > 0 {
			laneResult.MRR = count.reciprocalSum / float64(count.queries)
		}
		report.Lanes[lane] = laneResult
		totalExpected += count.expected
		totalFound += count.found
	}
	if totalExpected > 0 {
		report.Recall = float64(totalFound) / float64(totalExpected)
	}
	if totalHits > 0 {
		report.LanePrecision = float64(correctLane) / float64(totalHits)
		report.DistinctNodeRate = float64(distinctHits) / float64(totalHits)
	}
	return report, nil
}

func validateEvalCase(item evalCase, seenCase map[string]bool) error {
	if item.Schema != evalSchema {
		return fmt.Errorf("schema=%q，要求 %q", item.Schema, evalSchema)
	}
	if strings.TrimSpace(item.ID) == "" || len(item.ID) > 256 || seenCase[item.ID] {
		return fmt.Errorf("id 缺失、过长或重复")
	}
	if item.TopK < 1 || item.TopK > 100 {
		return fmt.Errorf("top_k=%d 越界(1..100)", item.TopK)
	}
	if len(item.QueryVector) == 0 || len(item.QueryVector) > 4096 {
		return fmt.Errorf("query_vector 维度越界(1..4096)")
	}
	if len(item.Records) == 0 || len(item.Records) > 100_000 {
		return fmt.Errorf("records 数量越界(1..100000)")
	}
	knownLanes := map[string]bool{"current": true, "risk": true, "history": true}
	for lane := range item.Qrels {
		if !knownLanes[lane] {
			return fmt.Errorf("qrels 包含未知 lane %q", lane)
		}
	}
	if len(item.ExpectedHits) != len(evalLanes) {
		return fmt.Errorf("expected_hits 必须显式包含 current/risk/history 三个 lane")
	}
	for lane := range item.ExpectedHits {
		if !knownLanes[lane] {
			return fmt.Errorf("expected_hits 包含未知 lane %q", lane)
		}
	}
	recordsByID := make(map[string]evalRecord, len(item.Records))
	expectedNodes := make(map[string]map[string]bool, len(evalLanes))
	for _, lane := range evalLanes {
		expectedNodes[lane] = make(map[string]bool)
	}
	for index, record := range item.Records {
		if record.ID == "" || record.NodeID == "" || recordsByID[record.ID].ID != "" {
			return fmt.Errorf("record %d 的 id/node_id 缺失或 id 重复", index)
		}
		recordsByID[record.ID] = record
		if !knownLanes[record.Lane] || !knownLanes[record.ExpectedLane] {
			return fmt.Errorf("record %q 的 lane/expected_lane 非法", record.ID)
		}
		if len(record.Vector) != len(item.QueryVector) {
			return fmt.Errorf("record %q 维度=%d，query=%d", record.ID, len(record.Vector), len(item.QueryVector))
		}
		expectedNodes[record.ExpectedLane][record.NodeID] = true
	}
	for _, lane := range evalLanes {
		expected, exists := item.ExpectedHits[lane]
		if !exists || len(expected) > item.TopK {
			return fmt.Errorf("expected_hits lane=%s 缺失或超过 top_k", lane)
		}
		seenIDs := make(map[string]bool, len(expected))
		seenNodes := make(map[string]bool, len(expected))
		for _, recordID := range expected {
			record, ok := recordsByID[recordID]
			if !ok || record.Lane != lane || seenIDs[recordID] || seenNodes[record.NodeID] {
				return fmt.Errorf("expected_hits lane=%s 的 record %q 缺失、串线、重复或节点未去重", lane, recordID)
			}
			seenIDs[recordID], seenNodes[record.NodeID] = true, true
		}
	}
	totalQrels := 0
	for _, lane := range evalLanes {
		seen := make(map[string]bool, len(item.Qrels[lane]))
		for _, nodeID := range item.Qrels[lane] {
			if nodeID == "" || seen[nodeID] || !expectedNodes[lane][nodeID] {
				return fmt.Errorf("qrels lane=%s 的 node %q 缺失、重复或无对应 expected_lane record", lane, nodeID)
			}
			seen[nodeID] = true
			totalQrels++
		}
	}
	if totalQrels == 0 {
		return fmt.Errorf("至少需要一条人工 qrel")
	}
	return nil
}

func printReport(out io.Writer, report evalReport) {
	fmt.Fprintf(out, "schema=%s cases=%d\n", report.Schema, report.Cases)
	for _, lane := range evalLanes {
		item := report.Lanes[lane]
		fmt.Fprintf(out, "%s: recall@k=%.3f mrr=%.3f found=%d/%d\n", lane, item.Recall, item.MRR, item.Found, item.Expected)
	}
	fmt.Fprintf(out, "overall: recall@k=%.3f lane_precision=%.3f distinct_node_rate=%.3f ranking_violations=%d\n",
		report.Recall, report.LanePrecision, report.DistinctNodeRate, report.RankingViolations)
}

func reportFailures(report evalReport, gate thresholds) []string {
	var failures []string
	for _, lane := range evalLanes {
		item := report.Lanes[lane]
		if item.Expected == 0 {
			failures = append(failures, fmt.Sprintf("%s lane has no qrels", lane))
		} else if item.Recall < gate.minRecall {
			failures = append(failures, fmt.Sprintf("%s recall@k %.3f < %.3f", lane, item.Recall, gate.minRecall))
		}
	}
	if report.LanePrecision < gate.minLanePrecision {
		failures = append(failures, fmt.Sprintf("lane precision %.3f < %.3f", report.LanePrecision, gate.minLanePrecision))
	}
	if report.DistinctNodeRate < gate.minDistinctNodeRate {
		failures = append(failures, fmt.Sprintf("distinct-node rate %.3f < %.3f", report.DistinctNodeRate, gate.minDistinctNodeRate))
	}
	if report.RankingViolations > gate.maxRankingViolations {
		failures = append(failures, fmt.Sprintf("ranking violations %d > %d", report.RankingViolations, gate.maxRankingViolations))
	}
	sort.Strings(failures)
	return failures
}

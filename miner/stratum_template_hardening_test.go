package main

import (
	"bufio"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
)

func TestMiner_FloatDifficultyToTargetEdgeCases(t *testing.T) {
	diffs := []float64{
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		-10.5,
		0,
		0.0001,
		1,
		1e200,
	}
	for _, d := range diffs {
		tgt := floatDifficultyToTarget(d)
		if tgt == nil || tgt.Sign() <= 0 || tgt.Cmp(core.MaxTarget256) > 0 {
			t.Fatalf("floatDifficultyToTarget(%v) returned out-of-range target: %v", d, tgt)
		}
	}
}

func TestStratum_ParseNotifyValidation(t *testing.T) {
	validTargetHex := core.DifficultyToTarget(1).Text(16)
	overMaxHex := new(big.Int).Add(core.MaxTarget256, big.NewInt(1)).Text(16)

	invalidCases := []struct {
		name   string
		params []interface{}
	}{
		{"too_few_params", []interface{}{"j1", "prev", "mr", "armx1...", validTargetHex}},
		{"blank_prevhash", []interface{}{"j1", "   ", "mr", "armx1...", validTargetHex, "1700000000"}},
		{"invalid_hex_target", []interface{}{"j1", "prev", "mr", "armx1...", "zzzz", "1700000000"}},
		{"zero_target", []interface{}{"j1", "prev", "mr", "armx1...", "0", "1700000000"}},
		{"over_max_target", []interface{}{"j1", "prev", "mr", "armx1...", overMaxHex, "1700000000"}},
		{"negative_height", []interface{}{"j1", "prev", "mr", "armx1...", validTargetHex, "1700000000", true, float64(-1)}},
	}

	for _, tc := range invalidCases {
		if job, err := parseStratumNotify(tc.params); err == nil {
			t.Fatalf("parseStratumNotify(%s) expected error, got %+v", tc.name, job)
		}
	}

	job, err := parseStratumNotify([]interface{}{
		"job-42", "abc123prev", "mr456", "armx1pool", validTargetHex, "1700000000", true, float64(42), "armaxhash-v2",
	})
	if err != nil {
		t.Fatalf("parseStratumNotify valid failed: %v", err)
	}
	if job.JobID != "job-42" || job.Height != 42 || !job.CleanJobs {
		t.Fatalf("unexpected parsed stratum job: %+v", job)
	}
}

func TestMiner_ValidateBlockTemplate(t *testing.T) {
	validHex := core.DifficultyToTarget(1).Text(16)
	overMaxHex := new(big.Int).Add(core.MaxTarget256, big.NewInt(1)).Text(16)

	invalid := []*BlockTemplate{
		nil,
		{Height: -1, PreviousBlockHash: "abc", CoinbaseValue: 100, Target: validHex},
		{Height: 1, PreviousBlockHash: "   ", CoinbaseValue: 100, Target: validHex},
		{Height: 1, PreviousBlockHash: "abc", CoinbaseValue: -1, Target: validHex},
		{Height: 1, PreviousBlockHash: "abc", CoinbaseValue: 100, Target: "not-hex"},
		{Height: 1, PreviousBlockHash: "abc", CoinbaseValue: 100, Target: "0"},
		{Height: 1, PreviousBlockHash: "abc", CoinbaseValue: 100, Target: overMaxHex},
		{Height: 1, PreviousBlockHash: "abc", CoinbaseValue: 100, Target: "", Difficulty: math.NaN()},
	}
	for i, tmpl := range invalid {
		if tgt, err := validateMinedBlockTemplate(tmpl); err == nil {
			t.Fatalf("case %d expected invalid template error, got target %v", i, tgt)
		}
	}

	tgt, err := validateMinedBlockTemplate(&BlockTemplate{
		Height:            1,
		PreviousBlockHash: "0000abc",
		CoinbaseValue:     60 * core.CoinUnit,
		Target:            validHex,
	})
	if err != nil {
		t.Fatalf("valid block template rejected: %v", err)
	}
	if tgt.Cmp(core.DifficultyToTarget(1)) != 0 {
		t.Fatalf("target mismatch: got %s", tgt.Text(16))
	}
}

func TestStratum_BoundedLineReadAndThreadClamp(t *testing.T) {
	okLine := strings.Repeat("a", 512) + "\n"
	got, err := readBoundedStratumLine(bufio.NewReader(strings.NewReader(okLine)), maxStratumLineBytes)
	if err != nil || got != okLine {
		t.Fatalf("expected valid bounded read, got len=%d err=%v", len(got), err)
	}

	oversized := strings.Repeat("b", maxStratumLineBytes+128) + "\n"
	if _, err := readBoundedStratumLine(bufio.NewReader(strings.NewReader(oversized)), maxStratumLineBytes); err == nil {
		t.Fatalf("expected error reading stratum line exceeding %d bytes", maxStratumLineBytes)
	}

	limit := core.MaxAllowedMiningThreads()
	threads, auto := GetOptimalCPUWorkerThreads(limit + 500)
	if auto {
		t.Fatalf("expected auto=false when explicit thread count was passed")
	}
	if threads < 1 || threads > limit {
		t.Fatalf("expected explicit threads to be clamped to [1, %d], got %d", limit, threads)
	}
}

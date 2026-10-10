package actuator

import (
	"testing"
)

// ============================================================
// 压缩循环的收敛性与终止性
// ============================================================
//
// 压缩循环是「选取 → 压缩 → 标记 covered → 重建 → 重算水位」的迭代。
// 完整跑它需要 DB 与 LLM，这里把「选取 + 标记 covered + 重建」这条
// 状态推进链路抽成纯函数模拟，验证两个必须成立的性质：
//
//  1. 终止性：任何输入下循环都会结束，不会无限空转。
//     漏了这条，一次用户请求会变成无数次 LLM 调用。
//  2. 单调推进：每轮至少吃掉一个 task，可压部分严格递减。
//     漏了这条，循环会在「压不动却判未达标」的状态下打转到轮次上限。

// simulateRounds 模拟压缩循环的段推进过程，返回每轮选中的段数。
//
// 每轮：按 pickSegmentsToCompact 选段 → 把选中的段从剩余集合里移除
// （等价于它们被标记 covered、下次重建不再出现）。
func simulateRounds(segs []historySegment, maxRounds int) []int {
	remaining := append([]historySegment{}, segs...)
	var picks []int

	for round := 0; round < maxRounds; round++ {
		detail := 0
		for i := range remaining {
			detail += remaining[i].Tokens
		}
		picked, _ := pickSegmentsToCompact(remaining, detail)
		if len(picked) == 0 {
			break
		}
		picks = append(picks, len(picked))
		remaining = remaining[len(picked):]
	}
	return picks
}

func TestCompactLoop_TerminatesAndMakesProgress(t *testing.T) {
	cases := map[string][]historySegment{
		"均匀十段": {
			seg("t1", 100), seg("t2", 100), seg("t3", 100), seg("t4", 100), seg("t5", 100),
			seg("t6", 100), seg("t7", 100), seg("t8", 100), seg("t9", 100), seg("t10", 100),
		},
		"首段巨大": {
			seg("huge", 900), seg("t2", 50), seg("t3", 50),
		},
		"末段巨大": {
			seg("t1", 50), seg("t2", 50), seg("huge", 900),
		},
		"两段": {
			seg("t1", 500), seg("t2", 500),
		},
		"单段": {
			seg("only", 1000),
		},
		"体量悬殊": {
			seg("tiny", 1), seg("big", 100000), seg("tiny2", 2),
		},
	}

	for name, segs := range cases {
		t.Run(name, func(t *testing.T) {
			// 给足够多的轮次上限：若逻辑有问题会跑满，说明不终止
			const generousRounds = 1000
			picks := simulateRounds(segs, generousRounds)

			if len(picks) >= generousRounds {
				t.Fatalf("循环未终止：跑满 %d 轮", generousRounds)
			}
			// 每轮必须至少吃掉一个段
			for i, n := range picks {
				if n <= 0 {
					t.Fatalf("第 %d 轮选中 %d 个段，未推进", i+1, n)
				}
			}
			// 所有段最终都应被吃掉（累计等于总段数）
			total := 0
			for _, n := range picks {
				total += n
			}
			if total != len(segs) {
				t.Errorf("累计选中 %d 段，应为全部 %d 段", total, len(segs))
			}
			t.Logf("%d 段 → %d 轮吃完，每轮 %v", len(segs), len(picks), picks)
		})
	}
}

func TestCompactLoop_RespectsMaxRounds(t *testing.T) {
	// 段数远多于轮次上限时，循环必须在上限处停下（按现状放行），
	// 而不是一直压到底 —— 否则一次请求会积累几十次 LLM 调用
	segs := make([]historySegment, 0, 200)
	for i := 0; i < 200; i++ {
		segs = append(segs, seg(string(rune('a'+i%26)), 10))
	}
	picks := simulateRounds(segs, maxCompactRounds)
	if len(picks) > maxCompactRounds {
		t.Fatalf("超出轮次上限：%d > %d", len(picks), maxCompactRounds)
	}
	t.Logf("200 段在 %d 轮上限内推进了 %v", maxCompactRounds, picks)
}

func TestCompactLoop_ConvergesFastOnUniformSegments(t *testing.T) {
	// 均匀分布下每轮吃掉 80%，收敛应该很快（几轮内吃完绝大部分）。
	// 这条断言防「选取过于保守导致要压十几轮」——那会让首次回答
	// 慢到不可接受。
	segs := make([]historySegment, 0, 20)
	for i := 0; i < 20; i++ {
		segs = append(segs, seg(string(rune('a'+i)), 1000))
	}
	picks := simulateRounds(segs, 100)

	// 第一轮就该吃掉 16 段（80%）
	if picks[0] != 16 {
		t.Errorf("首轮应吃掉 16/20 段，实际 %d", picks[0])
	}
	// 在 maxCompactRounds 轮内应吃掉九成以上
	eaten := 0
	for i := 0; i < len(picks) && i < maxCompactRounds; i++ {
		eaten += picks[i]
	}
	if eaten < 18 {
		t.Errorf("%d 轮内只吃掉 %d/20 段，收敛过慢", maxCompactRounds, eaten)
	}
	t.Logf("20 段均匀：每轮 %v", picks)
}

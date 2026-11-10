package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestKorean checks that the Korean UI fits every layout (its text is two
// cells a character) and that English is untouched without it.
func TestKorean(t *testing.T) {
	SetLanguage("ko")
	defer SetLanguage("")
	m := ownerModel(t, 200, 55)
	out := view(m)
	t.Logf("200x55:\n%s", out)
	assertContains(t, out, "─ 압력 ─", "주의", "원인: 커널 메모리 압력 경고", "▸ 에이전트 (53)  합계 CPU 339%, 9.0 GiB",
		"무거운 작업", "소유자별 메모리", "에이전트 53개 시작: claude-1", "tab 포커스", "q 종료")
	for _, size := range [][2]int{{200, 55}, {120, 40}, {100, 30}, {79, 24}, {60, 20}, {40, 10}} {
		m = update(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		assertFits(t, view(m), size[0], size[1])
		assertFits(t, view(press(t, m, "?")), size[0], size[1])
	}
	SetLanguage("")
	if out := view(m); strings.Contains(out, "에이전트") || !strings.Contains(out, "AGENTS") {
		t.Errorf("English view after SetLanguage(\"\"):\n%s", out)
	}
}

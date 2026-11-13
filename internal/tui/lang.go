package tui

// korean is the UI text in Korean, by its English text. Text from throtd
// (event messages, a degraded sampler's report) and names that are also
// configuration values (the scheduler modes, workload classes) stay as they
// are.
var korean = map[string]string{
	"PRESSURE": "압력", "MEMORY": "메모리", "SWAP": "스왑", "LATENCY": "지연", "SCHEDULER": "스케줄러",
	"MEMORY BY OWNER": "소유자별 메모리", "AGENTS": "에이전트", "HEAVY WORKLOADS": "무거운 작업",
	"EVENTS": "이벤트", "HELP": "도움말", "SYSTEM": "시스템", "DETAILS": "상세",
	"UNKNOWN": "알 수 없음", "NORMAL": "정상", "ELEVATED": "주의", "HIGH": "높음", "CRITICAL": "위험",
	"normal": "정상", "warning": "경고", "critical": "위험",

	"why: ": "원인: ", "latency ": "지연 ", "  latency ": "  지연 ", "pressure ": "압력 ",
	"load %.2f/cpu": "부하 %.2f/cpu", "load %.2f": "부하 %.2f", "load n/a": "부하 n/a",
	"kernel: ": "커널: ", "growth ": "증가 ", "foreground lag": "포그라운드 지연", "lag": "지연",
	"OFF": "꺼짐", "not scheduling": "스케줄링 안 함", "idle": "유휴",
	"limit %d (%d–%d)": "한도 %d (%d–%d)", "limit %d": "한도 %d",
	" (adaptive, start %d, max %d)": " (자동, 시작 %d, 최대 %d)", "per-agent %d": "에이전트당 %d",
	"%d would queue": "대기 예정 %d", "%d queued": "대기 %d", "%d reserved": "예약 %d",
	" %": " %", " % of RAM": " RAM 대비 %", " · last ": " · 최근 ", "of ": "전체 ", " used, % of RAM": " 사용, RAM 대비 %", "(no data yet)": "(아직 데이터 없음)", "(no data)": "(데이터 없음)",
	"● agents %s": "● 에이전트 %s", "  ○ outside %s": "  ○ 그 밖 %s",
	"(no agents detected)": "(감지된 에이전트 없음)",
	"+%d agents %s":        "+에이전트 %d개 %s", "+%d outside %s": "+그 밖 %d개 %s", "○ other ": "○ 기타 ",

	"starting…": "시작하는 중…",
	"terminal too small (%dx%d); throt needs at least %dx%d. Press q to quit.": "터미널이 너무 작습니다(%dx%d). throt은 최소 %dx%d가 필요합니다. q로 종료합니다.",
	"connecting to throtd at ":                               "throtd에 연결하는 중: ",
	"cannot reach throtd at %s: start it with `throtd`":      "throtd(%s)에 연결할 수 없습니다. `throtd`로 시작하세요",
	"retrying every %s; last error: %v":                      "%s마다 다시 시도합니다. 마지막 오류: %v",
	"throtd is not running — start it with `throtd`":         "throtd가 실행 중이 아닙니다. `throtd`로 시작하세요",
	"retrying every %s (socket %s)":                          "%s마다 다시 시도합니다 (소켓 %s)",
	"STOPPED  %d %s not paused by throtd (see throt doctor)": "STOPPED  throtd가 멈추지 않은 정지 %[2]s %[1]d개 (throt doctor 참고)",
	"processes": "프로세스", "process": "프로세스",

	"AGENT": "에이전트", "CHILDREN": "자식", "MEM": "메모리", "OWNER": "소유자", "PROCESS": "프로세스",
	"CLASS": "분류", "STATE": "상태",
	"  total %s CPU, %s":                                    "  합계 CPU %s, %s",
	"  VM %s busy, %s CPU · 1 slot":                         "  VM %s 사용 중, CPU %s · 슬롯 1개",
	"+%d more (each ≤%.0f%% CPU, ≤%s; %s CPU, %s together)": "+%d개 더 (각각 CPU ≤%.0f%%, ≤%s; 합계 CPU %s, %s)",
	"(no heavy workloads)":                                  "(무거운 작업 없음)",
	"QUEUED":                                                "대기", "PAUSED": "일시정지", "running (would queue)": "실행 중 (대기 예정)",
	"bg": "저순위", "running": "실행 중",
	"  (esc back)": "  (esc 뒤로)", "DETAILS  agent ": "상세  에이전트 ", "DETAILS  workload ": "상세  작업 ",
	"pid %d is no longer reported by throtd": "throtd가 pid %d를 더 이상 보고하지 않습니다",
	" %d-%d of %d":                           " %d-%d / %d", "nothing selected": "선택된 항목 없음",
	"executable": "실행 파일", "started": "시작", "children": "자식", "class": "분류", "owner": "소유자",
	"owner pid": "소유자 pid", "mem": "메모리",
	"  filter: %s%s (%d of %d)": "  필터: %s%s (%[4]d개 중 %[3]d개)", "  (events unavailable: ": "  (이벤트를 읽을 수 없음: ",
	"(no events)": "(이벤트 없음)",
	"(no event matches the filter; esc clears it)": "(필터에 맞는 이벤트가 없습니다. esc로 지웁니다)",
	"%d agents started":                            "에이전트 %d개 시작",
	"%d agents exited":                             "에이전트 %d개 종료",
	"discovered %d heavy workloads":                "무거운 작업 %d개 발견",
	"%d heavy workloads exited":                    "무거운 작업 %d개 종료",

	"pause": "일시정지", "resume": "재개", "paused": "일시정지함:", "resumed": "재개함:",
	" failed: ": " 실패: ", " cancelled": " 취소됨",
	"filter: type text  backspace delete  enter keep  esc clear": "필터: 글자 입력  backspace 지우기  enter 유지  esc 지우기",
	"esc close": "esc 닫기",
	"esc back  j/k move  p/r pause/resume  s sort":                           "esc 뒤로  j/k 이동  p/r 일시정지/재개  s 정렬",
	"tab focus  j/k move  p/r pause/resume  enter details  s sort  / filter": "tab 포커스  j/k 이동  p/r 일시정지/재개  enter 상세  s 정렬  / 필터",
	"tab focus  j/k move  enter open  s sort  / filter":                      "tab 포커스  j/k 이동  enter 열기  s 정렬  / 필터",
	"? help  q quit": "? 도움말  q 종료",

	"resource-aware scheduler for coding agents": "코딩 에이전트를 위한 자원 인식 스케줄러",
	"version ": "버전 ", "● connected to throtd": "● throtd에 연결됨", "○ waiting for throtd at ": "○ throtd를 기다리는 중: ",
	"any key to continue  q quit": "아무 키나 누르면 계속  q 종료",

	"no signals reported yet":            "아직 보고된 신호 없음",
	"all signals below thresholds":       "모든 신호가 임계값 아래",
	"signals back below exit thresholds": "신호가 해제 임계값 아래로 돌아옴",
	"memory %s+ used":                    "메모리 %s 이상 사용",
	"CPU %s+ busy":                       "CPU %s 이상 사용",
	"load ≥%s per CPU":                   "CPU당 부하 ≥%s",
	"foreground lag ≥%s":                 "포그라운드 지연 ≥%s",
	"swapping fast (≥%s)":                "스왑이 빠르게 증가 (≥%s)",
	"kernel memory pressure %s":          "커널 메모리 압력 %s",

	"q, ctrl+c   quit (throtd keeps running)":                                 "q, ctrl+c   종료 (throtd는 계속 실행)",
	"tab         switch focus: agents / heavy workloads (shift+tab too)":      "tab         포커스 전환: 에이전트 / 무거운 작업 (shift+tab도 가능)",
	"j/k, ↑/↓    move the selection":                                          "j/k, ↑/↓    선택 이동",
	"enter       agents: open the agent drilldown; heavy: details":            "enter       에이전트: 상세 화면 열기, 무거운 작업: 상세 보기",
	"esc         leave the drilldown; close details and help;":                "esc         상세 화면에서 나가기, 상세와 도움말 닫기,",
	"            else clear the events filter":                                "            그 밖에는 이벤트 필터 지우기",
	"/           filter the events by kind or text (enter keeps, esc clears)": "/           이벤트를 종류나 글자로 필터 (enter 유지, esc 지우기)",
	"p / r       pause / resume the selected heavy workload (asks y/n)":       "p / r       선택한 무거운 작업 일시정지 / 재개 (y/n 확인)",
	"s           sort the table: auto, cpu, mem, name, daemon order":          "s           표 정렬: 자동, cpu, 메모리, 이름, 데몬 순서",
	"?           toggle this help":                                            "?           이 도움말 열기/닫기",
	"Auto sorts by memory while pressure is from memory, else by CPU.":        "자동 정렬은 압력 원인이 메모리면 메모리순, 아니면 CPU순입니다.",
	"Agents beyond the first rows fold into a +N more row; j scrolls on.":     "첫 줄들을 넘는 에이전트는 +N개 더 줄로 접히고, j로 계속 내려갑니다.",
	"Sparklines and charts: the latest polls (one a second) of CPU,":          "스파크라인과 차트: 최근 측정값(1초에 한 번)의 CPU,",
	"memory used, swap (growth under SWAP), latency and pressure.":            "메모리 사용량, 스왑(스왑 패널은 증가량), 지연, 압력입니다.",
	"MEMORY BY OWNER: ● agents, ○ processes outside every agent.":             "소유자별 메모리: ● 에이전트, ○ 에이전트 밖의 프로세스.",
	"The TUI makes no decisions: p and r send only what you confirm,":         "TUI는 아무것도 결정하지 않습니다. p와 r은 확인한 것만 보내고,",
	"SWAP is the amount in use, drawn against the physical memory.":           "스왑은 사용량이고, 막대는 물리 메모리 대비입니다.",
	"and throtd checks ownership before acting.":                              "throtd가 실행 전에 소유권을 확인합니다.",
}

// lang is the text table in use; nil is English.
var lang map[string]string

// SetLanguage picks the UI language before the program starts: "ko" for
// Korean, anything else for English.
func SetLanguage(code string) {
	lang = nil
	if code == "ko" {
		lang = korean
	}
}

// tr returns s in the UI language, or s itself when it has no translation.
func tr(s string) string {
	if t, ok := lang[s]; ok {
		return t
	}
	return s
}

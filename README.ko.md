# Throt

[English](README.md) | **한국어**

[![CI](https://github.com/cfcromn/Throt/actions/workflows/ci.yml/badge.svg)](https://github.com/cfcromn/Throt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**로컬 AI 코딩 에이전트를 위한 리소스 인식 스케줄러(scheduler)와 TUI.**

> 머신을 녹이지 않고 더 많은 코딩 에이전트를 돌린다.

- [소개](#소개)
- [빠른 시작](#빠른-시작)
- [설치와 업데이트](#설치와-업데이트)
- [Throt 사용하기](#throt-사용하기)
- [설정](#설정)
- [판단 방식](#판단-방식)
- [문제 해결](#문제-해결)
- [로드맵](#로드맵)
- [기여와 라이선스](#기여와-라이선스)

## 소개

코딩 에이전트를 여러 개 동시에 돌리면 에이전트 자체는 대부분 모델 응답을 기다린다. 문제는 에이전트가 띄운 자식 프로세스끼리 부딪친다는 점이다. Gradle/Maven/Go/npm 빌드, Docker 빌드와 Compose 스택, 테스트 스위트, 언어 서버, 로컬 데이터베이스가 그렇다. 그 결과 CPU가 포화되고, 메모리 압박이 생기고, swap이 늘고, 포그라운드 작업이 굼떠진다.

Throt는 로컬에서 동작하는 Go 데몬(daemon, `throtd`)과 클라이언트(`throt`: CLI, 터미널 UI, MCP 서버)로 이루어진다. 코딩 에이전트의 프로세스 트리를 지켜보고, 무거운 작업(빌드, 테스트, 컨테이너 빌드)을 그 작업을 시작한 에이전트에 귀속시키고, 리소스 압력(pressure)을 측정한다. 사용자가 켜면 동시에 실행되는 무거운 작업의 수도 제한한다. 목표는 낮은 CPU 수치가 아니라 개발자가 체감하는 반응성과 실제로 쓸모 있는 처리량이다.

원칙:
- **제어하기 전에 관찰한다.** 스케줄러는 기본적으로 꺼져 있다. 처음에는 `observe` 모드로 켠다.
- **특정 에이전트에 묶이지 않는다.** 에이전트는 어댑터로 인식한다. Claude Code, Codex, OpenCode, Gemini, Aider, Cursor Agent, Goose, Amp, GitHub Copilot CLI, Orca를 기본 지원하고, 그 밖의 에이전트는 한 줄짜리 사용자 정의 규칙으로 추가한다([에이전트가 감지되지 않는다](#에이전트가-감지되지-않는다) 참고).
- **로컬 우선.** 텔레메트리는 없다. 네트워크에 접근하는 것은 사용자가 직접 실행한 `throt update`뿐이다.
- **실패하면 열어 둔다(fail open).** Throt가 죽거나 멈춰도 에이전트와 그 작업은 계속 진행된다.
- **모든 개입을 설명한다.** 대기열에 넣거나, 일시 정지하거나, 재개할 때마다 읽을 수 있는 이유가 남는다.

Throt는 코딩 에이전트도, 컨테이너 오케스트레이터도, 클라우드 스케줄러도 아니다. 프로세스를 죽이는 일도 없다. 일시 정지하고 재개할 뿐이다.

**상태: 1.0 릴리스 후보.** 최신 릴리스는 v1.0.0-rc.1이다. macOS와 Linux(amd64/arm64)를 지원한다. [호환성 정책](docs/ARCHITECTURE.md#compatibility)이 적용되므로 문서화된 동작은 1.x 안에서 깨지지 않는다.

## 빠른 시작

1. [GitHub CLI](https://cli.github.com)로 플랫폼(`darwin_arm64`, `darwin_amd64`, `linux_amd64`, `linux_arm64`)에 맞는 최신 릴리스를 **설치**하고, 검증한 뒤, 두 바이너리를 `PATH`에 둔다.

   ```bash
   V=$(gh release list -R cfcromn/Throt -L 1 --json tagName -q '.[0].tagName')
   gh release download "$V" -R cfcromn/Throt -p 'throt_*_darwin_arm64.tar.gz' -p checksums.txt
   shasum -a 256 --ignore-missing -c checksums.txt &&   # Linux: sha256sum --ignore-missing -c checksums.txt
     tar -xzf throt_*_darwin_arm64.tar.gz throt throtd &&
     mkdir -p ~/.local/bin && mv throt throtd ~/.local/bin/   # any directory on your PATH
   ```

2. 별도 터미널에서 **데몬을 시작**한다([로그인 시 자동 실행](#로그인-시-자동-실행)도 가능하다). 스케줄러를 켜기 전까지는 관찰만 한다.

   ```bash
   throtd
   ```

3. 다른 터미널에서 **상황을 살펴본다**.

   ```bash
   throt          # terminal UI: pressure, agents, heavy workloads; q quits
   throt status   # the same as plain text
   throt doctor   # diagnostics: socket path, config, daemon, missing metrics
   ```

4. **스케줄러를 observe 모드로 켠다.** 스케줄러는 자신이 할 일을 `scheduler_decision` 이벤트로 기록할 뿐, 실제로는 아무것도 하지 않는다.

   ```bash
   mkdir -p ~/.config/throt
   echo '{"scheduler": {"mode": "observe"}}' > ~/.config/throt/config.json
   throt config check   # ok, and: scheduler.mode off -> observe (file)
   throt reload         # applied: scheduler.mode off -> observe
   throt events -f      # watch; decisions appear when agents' heavy jobs compete for slots
   ```

5. 결정이 적절해 보이면 **enforce**로 바꾼다. 파일에서 `"mode": "enforce"`로 설정하고 `throt config check`와 `throt reload`를 다시 실행한다. 이제 한도를 넘는 새 무거운 작업은 슬롯이 빌 때까지 일시 정지(SIGSTOP)된다. 또 에이전트가 소유한 무거운 작업은 모두 백그라운드 CPU·I/O 우선순위로 돌기 때문에, 빌드가 모든 코어를 쓰고 있어도 내가 쓰는 앱은 계속 잘 반응한다. `throtd`를 멈추면 그동안 일시 정지한 작업을 모두 재개하고 낮췄던 우선순위도 되돌린다. 다만 Linux에서는 보통 nice 값이 프로세스가 끝날 때까지 남는다(`scheduler.background` 참고).

## 설치와 업데이트

### 설치

macOS와 Linux(amd64/arm64)용 릴리스 아카이브는 [Releases](https://github.com/cfcromn/Throt/releases)에 있다. 내려받는 방법은 [빠른 시작](#빠른-시작)에 나와 있다. 지금은 저장소가 비공개라서 내려받으려면 GitHub 로그인(`gh auth login`)이 필요하다.

아카이브는 릴리스의 `checksums.txt`로 검증한다.

```bash
shasum -a 256 --ignore-missing -c checksums.txt   # Linux: sha256sum --ignore-missing -c checksums.txt
gh attestation verify throt_<version>_<os>_<arch>.tar.gz --repo cfcromn/Throt   # only once the repository is public
```

`gh attestation verify`에 쓰는 빌드 출처 증명(provenance)은 저장소가 공개 상태일 때만 게시된다.

**macOS Gatekeeper.** 바이너리는 서명도 공증(notarization)도 받지 않았다. 브라우저로 내려받으면 macOS가 실행을 거부한다. `xattr -d com.apple.quarantine throt throtd`로 격리 속성을 한 번 지우면 된다. `gh release download`나 `curl`로 받은 아카이브, 그리고 `throt update`가 설치한 바이너리에는 이 속성이 붙지 않는다.

`go.mod`에 적힌 Go 버전으로 소스에서 빌드하려면 다음을 실행한다.

```bash
GOPRIVATE='github.com/cfcromn/*' go install github.com/cfcromn/Throt/cmd/throt@latest
GOPRIVATE='github.com/cfcromn/*' go install github.com/cfcromn/Throt/cmd/throtd@latest
```

저장소가 비공개인 동안 `go install`에는 다음 두 가지가 모두 필요하다.
- `GOPRIVATE='github.com/cfcromn/*'`(또는 한 번만 `go env -w GOPRIVATE='github.com/cfcromn/*'`). 그래야 Go가 이 모듈을 볼 수 없는 공개 모듈 프록시와 체크섬 데이터베이스를 거치지 않고 직접 가져온다.
- 저장소를 읽을 수 있는 Git 자격 증명. Go는 `git`으로 모듈을 가져오기 때문이다. GitHub CLI에 로그인되어 있다면 `gh auth setup-git`으로 설정한다.

둘 중 하나라도 없으면 `go install`이 실패한다. 보통 프록시에서 `not found`가 나오거나 Git에서 `terminal prompts disabled`가 나온다.

`throt`와 `throtd`는 같은 릴리스에서 나온 것이어야 한다.

### 업데이트와 롤백

| 명령 | 하는 일 |
|---|---|
| `throt update --check` | 현재 버전과 최신 버전을 출력한다. 최신이면 0, 업데이트가 있으면 10으로 종료한다. |
| `throt update` | `throt`와 `throtd`를 내려받고, 검증하고, 교체한다. 먼저 확인을 묻고, `--yes`를 주면 묻지 않는다. |
| `throt update --version v0.9.0-rc.1` | 특정 릴리스를 설치한다. 더 새 버전이든 옛 버전이든 된다(이것이 롤백이다). |
| `throt update --pre` | 프리릴리스도 포함한다(실행 중인 버전이 프리릴리스면 기본값이다). |

`throt update`는 실행 중인 `throt`(심볼릭 링크는 따라간다)와 그 옆의 `throtd`를 찾는다. 그다음 현재 OS와 아키텍처용 아카이브를 `checksums.txt`와 함께 내려받고, SHA-256이 일치하지 않으면 진행하지 않는다. 새 바이너리 두 개를 실행해 버전을 확인한 뒤 `throtd`를 먼저, `throt`를 마지막으로 이름을 바꿔 제자리에 놓는다. 그 전 단계에서 실패하거나 쓰기 권한이 없는 디렉터리라면 기존 바이너리는 그대로 남는다. `--yes` 없이 stdin이 터미널이 아니면 진행을 거부한다. `go install`이나 개발 빌드로 설치한 바이너리는 건드리지 않고, 대신 `go install` 명령을 출력한다.

저장소가 비공개인 동안에는 GitHub 토큰이 필요하다. `GH_TOKEN`이나 `GITHUB_TOKEN`을 쓰고, 둘 다 없으면 GitHub CLI에 로그인되어 있을 때 `gh auth token`을 쓴다. 어떤 Throt 명령도 스스로 업데이트를 확인하지 않는다.

**업데이트 후에는 `throtd`를 재시작한다.** 실행 중인 데몬은 옛 버전 그대로이고, `throt update`는 재시작 명령을 출력한다. launchd는 `launchctl kickstart -k gui/$(id -u)/dev.throt.throtd`, systemd는 `systemctl --user restart throtd`를 쓰고, 그 밖에는 멈췄다가 다시 시작한다. `throtd`를 멈추면 그동안 일시 정지한 작업을 모두 재개한다.

손으로 업데이트하려면 `throtd`를 멈추고, 검증한 아카이브로 두 바이너리를 교체한 뒤 다시 시작한다. 한 번 게시한 버전은 다시 쓰지 않고, 수정 사항은 새 릴리스로 낸다. 설정은 같은 마이너 시리즈 안에서 하위 호환된다.

### 로그인 시 자동 실행

`throtd`는 SIGTERM이나 SIGINT를 받으면 자신이 일시 정지한 것을 모두 재개한다. 그래서 서비스 관리자가 안전하게 멈출 수 있다.

**macOS(launchd):** `~/Library/LaunchAgents/dev.throt.throtd.plist`로 저장하고, 두 경로를 자신의 경로로 바꾼 뒤(launchd는 `~`를 확장하지 않는다) `launchctl load ~/Library/LaunchAgents/dev.throt.throtd.plist`를 실행한다. 소켓 때문에 plist에 환경 변수를 넣을 필요는 없다. 로그인 세션의 launch agent는 셸과 같은 사용자별 `TMPDIR`(`/var/folders` 아래의 `confstr(_CS_DARWIN_USER_TEMP_DIR)` 디렉터리)를 받고, 둘 다 `XDG_RUNTIME_DIR`를 설정하지 않으므로 같은 기본 소켓을 쓴다.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.throt.throtd</string>
  <key>ProgramArguments</key><array><string>/Users/you/.local/bin/throtd</string></array>
  <key>StandardErrorPath</key><string>/Users/you/Library/Logs/throtd.log</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
```

**Linux(systemd 사용자 유닛):** `~/.config/systemd/user/throtd.service`로 저장하고 `systemctl --user enable --now throtd`를 실행한다(로그: `journalctl --user -u throtd`). systemd 사용자 매니저는 유닛에 `XDG_RUNTIME_DIR=/run/user/UID`를 준다. 로그인 세션에 `pam_systemd`가 설정하는 값과 같으므로 둘 다 같은 소켓을 쓴다.

```ini
[Unit]
Description=Throt resource-aware scheduler for coding agents

[Service]
ExecStart=%h/.local/bin/throtd
Restart=on-failure

[Install]
WantedBy=default.target
```

로그인 시 시작한 `throtd`에 `throt`가 여전히 닿지 않는다면(셸이 `XDG_RUNTIME_DIR`나 `TMPDIR`를 따로 설정하거나, SSH, `su`, `sudo`로 작업하는 경우) 환경 변수를 더 늘리지 말고 설정 파일에 절대 경로 `socket_path`를 지정한다. `throtd`와 모든 클라이언트는 같은 파일을 읽는다. 셸의 rc 파일을 읽지 않는 앱이 띄운 `throt mcp`도 마찬가지다. ["cannot reach throtd"](#cannot-reach-throtd)를 참고한다.

## Throt 사용하기

### 명령어

| 명령 | 하는 일 |
|---|---|
| `throt`, `throt top` | 터미널 UI. 종료해도 `throtd`에는 영향이 없다. |
| `throt status [--json]` | 압력과 그 이유, 호스트 리소스, 에이전트, 무거운 작업, 스케줄러 상태, 정지된 프로세스. |
| `throt status --idle` | 모든 에이전트 세션을 메모리 순으로 나이, CPU, 유휴 여부(세 번의 샘플 동안 CPU 1% 미만이고 무거운 작업이 없음)와 함께 보여 준다. 보고만 하며 프로세스를 멈추지 않는다. |
| `throt stats [--json]` | 에이전트별 CPU 시간, 최대 메모리, 무거운 작업 수, `throtd` 시작 이후 대기한 시간. |
| `throt learned [--forget SIGNATURE \| --clear]` | `throtd`가 무겁다고 학습한 명령 목록. Claude Code 훅은 이 명령들 앞에서도 기다린다. `--forget`이나 `--clear`로 지운다. [학습한 무거운 명령](#학습한-무거운-명령) 참고. |
| `throt events [-f] [-n N]` | 최근 데몬 이벤트를 JSON lines로 출력한다(기본 50개). `-f`를 주면 새 이벤트를 계속 출력한다. |
| `throt explain PID` | 무거운 작업이 실행 중이거나, 일시 정지되었거나, 대기 중인 이유. |
| `throt pause PID` | 내 에이전트가 소유한 무거운 작업을 일시 정지(SIGSTOP)한다. |
| `throt resume [--force] PID` | 다시 재개한다. Throt가 일시 정지하지 않은 정지 프로세스도 재개한다. [문제 해결](#프로세스가-정지-상태t에-멈춰-있다)을 참고한다. |
| `throt run [--timeout D] [--no-wait] -- CMD` | 무거운 작업 슬롯이 빌 때까지 기다린 뒤 `CMD`를 실행한다. [`throt run`](#시작-전에-기다리기-throt-run)을 참고한다. |
| `throt doctor` | 버그 리포트에 붙여 넣을 수 있도록 민감 정보를 걸러 낸 진단 정보. `throtd` 없이도 동작한다. |
| `throt ping` | `throtd`에 닿는지 확인하고 그 버전과 프로토콜을 출력한다. |
| `throt reload` | `throtd`가 설정을 다시 읽게 한다(SIGHUP을 보내는 것과 같다). |
| `throt config check` | 설정 파일을 검증하고, 무엇이 바뀌는지 보여 주고, 위험한 설정을 경고한다. |
| `throt config show [--json]` | 실제로 적용되는 설정과 각 값의 출처. |
| `throt version [--json]` | 클라이언트 버전. `--json`을 주면 프로토콜, Go, `throtd` 버전도 출력한다. |
| `throt update` | 더 새(또는 더 옛) 릴리스를 설치한다. [업데이트와 롤백](#업데이트와-롤백)을 참고한다. |
| `throt mcp` | 코딩 에이전트용 읽기 전용 MCP 서버(stdio). |
| `throt setup claude-code\|codex [--write]` | 코딩 에이전트에 붙여 넣을 설정을 출력한다. `--write`를 주면 대신 홈 디렉터리에 직접 설치한다(추가만 하고, 바꾸는 파일마다 `.throt-backup`을 남긴다). |
| `throt hook claude-code [--max-wait D] [--defer]` | Claude Code 훅(hook). |
| `throt wait [--timeout D]` | 호출한 에이전트 몫으로 무거운 작업 슬롯이 잡힐 때까지 기다린다. 훅의 `--defer`와 함께 쓴다(기본 30m). |
| `throt completion bash\|zsh\|fish` | 셸 자동 완성 스크립트. |

전역 플래그는 명령 앞에 둔다. `-config PATH`는 다른 설정 파일을 쓰고, `-n N`은 `throt events`가 출력할 이벤트 수를 정한다. `throt -h`는 모든 명령을 나열하고, 명령 뒤에 `-h`를 붙이면 그 명령의 사용법을 출력한다. `throtd -h`는 데몬의 플래그를 나열하고, `throtd -version`은 데몬 버전을 출력한다.

종료 코드: 성공하면 0, 오류는 1, 사용법 오류는 2, `throt update --check`에서 업데이트가 있으면 10이다. `throt run`은 실행한 명령의 종료 상태를 그대로 쓴다(명령을 실행할 수 없으면 126, 127). `throt hook claude-code`는 항상 0으로 종료한다. JSON 출력, 이벤트 종류, reason 코드, 종료 코드는 [Compatibility](docs/ARCHITECTURE.md#compatibility)에 정리되어 있다.

셸 자동 완성: 셸의 rc 파일에 `source <(throt completion zsh)`(또는 `bash`)를 추가하거나 `throt completion fish | source`를 실행한다. `pause`, `resume`, `explain` 뒤에서 무거운 작업의 PID를 완성한다.

### 터미널 UI

```text
$ throt
╭─ PRESSURE ────────╮ ╭─ CPU ──────────╮ ╭─ MEMORY ──────╮ ╭─ SWAP ────────╮ ╭─ SCHEDULER ─────────╮
│  ELEVATED         │ │ 31%            │ │ 88%           │ │ 91%           │ │ OBSERVE             │
│ why: kernel memo… │ │ load 0.62/cpu  │ │ 21.1/24.0 GiB │ │ 16.4/18.0 GiB │ │ limit 3 (2–10)      │
│ latency 6ms       │ │                │ │ kernel: warn… │ │ growth 4 MiB… │ │ 0 would queue       │
│ ▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃ │ │ ▃▄▄▅▅▅▆▆▆▆▅▅▅▅ │ │ ▇▇▇▇▇▇▇▇▇▇▇▇▇ │ │ █████████████ │ │ 0 reserved          │
╰───────────────────╯ ╰────────────────╯ ╰───────────────╯ ╰───────────────╯ ╰─────────────────────╯
╭─ CPU % · last 5m ─────────────╮ ╭─ MEMORY % · last 5m ──────────╮ ╭─ SWAP % · last 5m ───────────╮
│ 100%┤                         │ │ 100%┤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣴⣶ │ │ 100%┤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣤⣶⣶⣶⣶⣶⣶⣶⣶⣶⣶⣶⣶⣶ │
│  50%┤⣶⣿  ⢰⣾⣇  ⣴⣾⡄ ⢠⣷⣶  ⣸⣷⡆  ⣿ │ │  50%┤⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿ │ │  50%┤⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿ │
│   0%┤⣿⣿⣿⣰⣾⣿⣿⣧⣶⣿⣿⣿⣶⣾⣿⣿⣶⣼⣿⣿⣷⣆⣿⣿ │ │   0%┤⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿ │ │   0%┤⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿ │
╰───────────────────────────────╯ ╰───────────────────────────────╯ ╰──────────────────────────────╯
╭─ MEMORY BY OWNER ────────────────╮ ┏━ ▸ AGENTS (53)  total 339% CPU, 9.0 GiB ━━━━━━━━━━━━━━━━━━━━┓
│ ● agents 9.0 GiB  ○ outside 9.9… │ ┃   AGENT        PID   CPU     MEM▼  CHILDREN                 ┃
│ ● codex-1        ██████  2.2 GiB │ ┃ > codex-1    41350  190%  2.2 GiB        14                 ┃
│ ○ com.apple.Vir… █████▉  2.2 GiB │ ┃   codex-2    41357   42%  1.1 GiB         6                 ┃
│ ● codex-2        ██▉░░░  1.1 GiB │ ┃   codex-3    41364    8%  640 MiB         3                 ┃
│ ○ Browser ×19    ██░░░░  744 MiB │ ┃   claude-5   41028    2%  112 MiB         2                 ┃
│ ○ Orca ×13       █▊░░░░  650 MiB │ ┃   claude-10  41063    1%  112 MiB         2                 ┃
│ ● codex-3        █▋░░░░  640 MiB │ ┃   +48 more (each ≤3% CPU, ≤112 MiB; 96% CPU, 4.9 GiB toget… ┃
│ ○ WindowServer   █▋░░░░  610 MiB │ ┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛
│ ○ Slack Helper … █▏░░░░  420 MiB │ ╭─ HEAVY WORKLOADS ───────────────────────────────────────────╮
│ ● claude-5       ▎░░░░░  112 MiB │ │   OWNER  PID  PROCESS   CLASS   CPU  MEM▼  STATE            │
│ +49 agents 5.0 GiB · ○ other 5.… │ │   (no heavy workloads)                                      │
╰──────────────────────────────────╯ ╰─────────────────────────────────────────────────────────────╯
╭─ EVENTS ─────────────────────────────────────────────────────────────────────────────────────────╮
│ 09:00:09  SCHED  queued gradle (pid 2)                                                           │
│ 09:00:09  SCHED  queued gradle (pid 1)                                                           │
│ 09:00:09         pressure normal -> elevated                                                     │
╰──────────────────────────────────────────────────────────────────────────────────────────────────╯
tab focus  j/k move  enter open  s sort  / filter  ? help  q quit
```

기본 화면은 창을 가득 채우는 패널 대시보드다. 맨 윗줄에는 신호마다 통계 패널이 하나씩 있다. `PRESSURE`(수준을 해당 색으로 표시하고 그 이유를 보여 준다), `CPU`, `MEMORY`, `SWAP`, `LATENCY`(120열 이상에서 표시하고, 그보다 좁으면 `PRESSURE` 패널 안에 들어간다), `SCHEDULER`(모드, 한도, 대기 중인 작업과 예약된 작업)다. 값은 70%부터 노란색, 85%부터 빨간색이 되고, 140열 이상에서는 큰 숫자로 그린다. 패널마다 맨 아래에 최근 갱신(초당 한 번)의 스파크라인이 있다. `SWAP`은 swap 증가량, `PRESSURE`는 수준을 그린다. 그 아래에는 CPU, 메모리, swap 사용량을 지난 5분 동안 차트로 그린다. 이 기록은 TUI가 직접 보관하므로 종료하면 사라진다. `MEMORY BY OWNER`는 에이전트(`●`)와 어느 에이전트에도 속하지 않은 프로세스(`○`, 이름별로 묶고 최대 5개 이름까지, 인자나 경로는 표시하지 않는다)를 큰 순서대로 한 막대 차트에 놓고 나머지는 합산한다. 그래서 에이전트로 설명되지 않는 메모리가 눈에 띈다. 그 옆에 `AGENTS`와 `HEAVY WORKLOADS` 표가 있고(포커스된 표는 굵은 테두리와 `▸`로 표시한다), 남은 높이는 `EVENTS`가 차지한다.

처음에는 짧은 시작 화면(버전과 `throtd` 응답 여부)이 뜨고, 아무 키나 누르면 사라진다. `throtd`가 응답하고 1.5 s가 지나도 사라진다. CPU는 코어 하나 기준 백분율이다(코어 두 개가 바쁘면 200%). 에이전트의 CPU와 메모리에는 그 에이전트가 소유한 모든 프로세스가 포함된다. 압력의 이유가 메모리나 swap 신호이면 표를 메모리 기준으로, 아니면 CPU 기준으로 정렬한다. 처음 9개를 넘는 에이전트는 `+N more` 한 줄로 접히고, 선택을 그 아래로 옮기면 전체를 스크롤할 수 있다. 같은 초에 에이전트나 무거운 작업이 한꺼번에 나타나거나 사라지면 이벤트 한 줄로 묶는다. 플랫폼이 제공하지 않는 지표는 `n/a`로 표시한다. `throtd`가 실행 중이 아니면 TUI가 그렇게 알리고, 데몬이 시작되면 다시 연결한다. 80열보다 좁으면 패널을 한 열로 쌓고, CPU, 메모리, swap은 `SYSTEM` 패널 안에 막대로 표시한다. 터미널 높이가 낮으면 차트, 이벤트, 메모리 패널부터 빼고, 행이 더 모자라면 테두리도 뺀다. 40×10보다 작으면 창을 키우라고 안내한다.

`STOPPED` 줄은 throtd가 일시 정지하지 않은 프로세스의 수를 센다(`throt doctor` 참고). 이벤트에는 `CTL`, `SCHED`, `FAIL` 태그가 붙는다. 모든 상태는 색뿐 아니라 텍스트로도 표시하고, 색은 밝은 배경과 어두운 배경에서 모두 대비를 유지한다. `NO_COLOR`나 `TERM=dumb`를 설정하면 색을 완전히 끈다.

| 키 | 동작 |
|---|---|
| `tab` | 에이전트 표와 무거운 작업 표 사이를 전환한다. |
| `j`/`k`, `↓`/`↑` | 선택을 옮긴다. |
| `enter` | 에이전트에서: 상세 화면을 연다(PID, CPU, 메모리, 자식 프로세스, 그리고 그 에이전트가 소유한 무거운 작업별 상태: running, `PAUSED`, `QUEUED`). 무거운 작업에서: 상세 정보를 펼치거나 접는다. |
| `esc` | 상세 화면, 도움말, 필터에서 나간다. |
| `p` / `r` | 선택한 무거운 작업을 `y/n` 확인 후 일시 정지하거나 재개한다. |
| `s` | 정렬 열을 순환한다: auto(메모리 압력이면 메모리, 아니면 CPU), CPU, 메모리, 이름, 그리고 `throtd`의 순서(`▼` 내림차순, `▲` 오름차순). |
| `/` | 이벤트를 종류나 텍스트로 필터링한다(대소문자를 구분하지 않는 부분 문자열). `enter`로 유지하고 `esc`로 지운다. |
| `?` | 도움말. |
| `q`, `ctrl+c` | 종료. |

### 코딩 에이전트: MCP, Claude Code 훅, Codex

**MCP.** `throt mcp`는 stdio로 동작하는 읽기 전용 [MCP](https://modelcontextprotocol.io) 서버다. 에이전트는 큰 빌드를 시작하기 전에 이 서버로 압력과 여유 용량을 확인할 수 있다. 제공하는 도구는 다음과 같다. `throt_status`(압력, 리소스, 에이전트, 무거운 작업), `throt_capacity`(이유와 제안이 담긴 권고용 `ok_to_start`. 기본 대상은 서버를 띄운 에이전트다), `throt_events`, `throt_stats`, `throt_explain`(PID가 실행 중이거나, 일시 정지되었거나, 대기 중인 이유), `throt_wait_for_capacity`(용량 판정을 초당 한 번씩 최대 `max_wait_seconds` 동안 확인한다. 범위 1–600, 기본 60, 취소 가능). 어느 도구도 일시 정지, 재개, 설정 변경을 하지 않는다. `throtd`가 실행 중이어야 한다. 도구 출력은 프로세스 이름과 PID를 포함해 에이전트의 컨텍스트에 들어가므로 모델 제공자에게도 전달된다. 서버는 MCP `2026-07-28`(요청별 버전을 `_meta`에 담고, `server/discover` 지원)과 `initialize` 리비전 `2025-11-25`, `2025-06-18`, `2024-11-05`를 지원하며, 도구만 제공한다. 모든 도구는 텍스트와 구조화된 콘텐츠를 반환하고, 그 형식은 `outputSchema`에 기술되어 있다([필드](docs/ARCHITECTURE.md#compatibility)).

```bash
claude mcp add --scope user throt -- throt mcp
```

**Claude Code 훅.** `throt setup claude-code`는 붙여 넣을 `settings.json` 훅과 MCP 설정을 출력한다. `throt setup claude-code --write`는 훅을 `~/.claude/settings.json`에 직접 넣는다. 이미 `throt hook claude-code`를 실행하는 훅이 있으면 아무것도 하지 않는다. 다른 설정은 모두 그대로 두고(키는 정렬되고 들여쓰기가 바뀐다), 이전 파일은 `settings.json.throt-backup`으로 남기며, 파일이 올바른 JSON이 아니면 아무것도 바꾸지 않고 거부한다. 이 훅(`throt hook claude-code --max-wait D`, `Bash`에 거는 [`PreToolUse`](https://code.claude.com/docs/en/hooks) 훅)이 있으면 따로 지시하지 않아도 Claude Code가 여유 용량을 기다린다.
- Bash 명령이 무거운 도구 하나이거나 `throtd`가 [무겁다고 학습한](#학습한-무거운-명령) 명령일 때만 기다린다(무거운 도구는 Throt가 무겁다고 분류하는 이름 기준이며, `gradlew`·`mvnw` 래퍼와 `docker build`, `docker buildx build`, `docker compose build`도 포함한다). 앞에 `cd DIR &&`, 변수 할당, `time`이 붙어도 된다. 이 경우 호출한 에이전트가 무거운 작업을 시작해도 될 때까지, 또는 `D`가 지날 때까지 `throtd`를 초당 한 번 확인한다(기본 50s. 훅이 타임아웃되면 호출이 막히므로 setup 스니펫은 120s 훅 `timeout`과 함께 110s를 쓴다).
- 그 밖의 경우는 바로 통과한다. 파이프, `;`, `||`, `&`, 리다이렉션, 서브셸, 치환, heredoc, 주석, 다른 명령 뒤의 무거운 도구(`make && ./run`), `throt run` 자체, 파싱할 수 없는 입력, 그리고 `--version`이나 `go env` 같은 [가벼운 호출](#claude-code-훅-때문에-명령-실행이-늦어진다)이 그렇다. `throtd`가 꺼져 있을 때도 모두 통과한다. 이 중 `throtd`에 연락하는 것은 자주 쓰는 가벼운 도구(`ls`, `git`, `grep`, `cat` 등)가 아닌 단순 명령 하나뿐이고, 그것도 시그니처를 알리려고 한 번 연락할 뿐이다.
- 명령을 바꾸거나, 승인하거나, 거부하지 않는다. 따라서 권한 규칙은 그대로 적용된다. 기다린 뒤에는 한 줄짜리 `systemMessage`를 보여 준다.
- 권고일 뿐이다. 슬롯을 예약하지 않으므로 같은 순간에 통과한 두 명령이 함께 시작될 수 있다. 슬롯을 예약하려면 `throt run --`을 쓴다. `throt run`에 대한 권한 allow 규칙은 추가하지 않는다. 그런 규칙은 `throt run`을 거쳐 실행되는 모든 명령을 허용하게 된다.

**기다리지 않고 미루기(선택 사항).** `throt hook claude-code --defer`를 쓰면 빈 슬롯이 없을 때 훅이 기다리지 않는다. `throtd`가 에이전트의 대기 순번을 잡아 두고(큐에 들어간 워크로드, `throt run` 호출과 같은 줄이다), 훅은 호출을 거부하면서 그 이유로 Claude에게 몇 번째인지 알리고, 그동안 다른 작업을 하다가 `throt wait`를 백그라운드로 실행해 두고 그것이 끝나면 같은 명령을 다시 실행하라고 전한다. `throt wait`는 에이전트 몫으로 슬롯이 잡히면 0으로 끝난다. 잡힌 슬롯은 에이전트의 다음 무거운 명령을 60s 동안 기다리고, 훅은 그 명령을 바로 통과시킨다. 처음부터 슬롯이 비어 있을 때도 그 명령 몫으로 슬롯을 잡아 둔다. 미뤄진 호출이나 `throt wait`가 10분 동안 갱신하지 않은 순번은 사라지고, 잡아 둔 슬롯을 쓰지 않으면 60s 뒤에 풀린다. 오류가 나거나 `throtd`가 이 기능보다 오래된 버전이면 훅은 위에서 설명한 대로 기다린다. `throt setup`은 아직 `--defer`를 넣지 않으므로 `settings.json`의 훅 명령에 직접 추가한다.

#### 학습한 무거운 명령

`python train.py`, `./scripts/build.sh`, `npx playwright test`처럼 Throt가 이름으로 모르는 명령은 실행된 뒤에야 무거운 작업(class `command`)으로 잡힌다. 그때 빌드를 일시 정지해도 이미 잡은 메모리는 그대로 남는다. 그래서 `throtd`가 이런 명령을 학습한다. 훅(과 `throt run`)은 단순 명령 하나를 실행하기 전에 그 명령의 *시그니처*를 `throtd`에 알린다. 시그니처는 프로그램의 base name과, 플래그가 아닌 첫 인자의 base name이다(`python train.py`, `build.sh`, `npx playwright`). 알린 뒤 10초 안에 그 에이전트의 무거운 작업이 시작되고, 그 작업을 띄웠을 만한 다른 보고가 그 에이전트에 없으면 시그니처를 학습한다. 그 뒤로 훅은 `make`처럼 이 명령 앞에서도 여유 용량을 기다린다. 학습한 명령이 무거워지지 않고 연달아 세 번 실행되면 잊는다. 최대 256개를 관찰한 최대 메모리와 함께 소켓 옆의 `learned.json`에 보관하므로 재시작해도 남는다(가득 차면 가장 오래전에 무거웠던 것부터 지운다).

`throt learned`로 목록을 보고, `throt learned --forget 'python train.py'`나 `--clear`로 지운다. 학습에는 `throtd` 1.0.0-rc.1 이상이 필요하다. 더 오래된 `throtd`이거나 `throtd`가 꺼져 있으면 훅은 예전과 똑같이 동작한다. 훅이 실행되고 한참 뒤에(예를 들어 권한 승인을 기다리느라) 시작된 명령은 학습하지 않는다.

**Codex.** `throt setup codex`는 `~/.codex/config.toml`에 넣을 블록과 `AGENTS.md`용 문단을 출력한다. `throt setup codex --write`는 둘을 각각 `~/.codex/config.toml`과 `~/.codex/AGENTS.md`(`$CODEX_HOME`이 있으면 그 아래)에 덧붙인다. 이미 들어 있으면 건너뛰고, 백업도 같은 방식으로 남긴다.

```toml
[mcp_servers.throt]
command = "throt"
args = ["mcp"]
```

#### Gradle, Maven, Docker

에이전트가 `./gradlew test`나 `./mvnw verify`를 돌리고, 그 과정에서 Testcontainers가 Docker에 데이터베이스를 띄우는 경우가 많다면 시작 전에 막는 설정을 해 둔다.

```bash
throt setup claude-code --write
throt setup codex --write
mkdir -p ~/.gradle
printf '\norg.gradle.daemon=false\nkotlin.compiler.execution.strategy=in-process\n' >> ~/.gradle/gradle.properties
```

그리고 `throtd`를 `scheduler.mode` `enforce`와 작은 고정 한도로 실행한다. 예를 들면 `"heavy_limit": 3, "adaptive": false, "per_agent_limit": 1`이다. 메모리를 지키는 건 빌드를 시작 전에 붙잡아 두는 쪽이다. 아직 아무것도 할당하지 않았기 때문이다. 이미 시작한 빌드는 일시 정지해도 메모리를 전부 쥐고 있다. Gradle은 보통 빌드를 부모가 `launchd`나 `init`인 분리된 데몬에 넘기므로, Throt는 그 빌드가 어느 에이전트 것인지 알 수 없어 대기열에 넣지 못한다. `org.gradle.daemon=false`로 두면 빌드가 에이전트 아래에서 돈다(`throt doctor`가 바쁜 미귀속 `java`를 짚어 준다). Kotlin Gradle 플러그인은 그와 상관없이 자체 컴파일러 데몬을 띄우는데, `kotlin.compiler.execution.strategy=in-process`로 두면 컴파일러가 빌드 안에서 돈다. 컨테이너는 Docker VM 안에서 돌아 Throt에는 프로세스가 아예 보이지 않으니, 컨테이너를 띄우는 `./gradlew test`를 막는 것이 유일한 방법이다.

### 시작 전에 기다리기: `throt run`

`throt run -- CMD [ARGS...]`는 명령을 시작하기 *전에* 무거운 작업 슬롯을 요청한다. 일시 정지당하는 것보다 부드러운 대안이다. 무거운 작업을 시작해도 될 때까지(압력이 high 미만이고, 빈 슬롯이 있고, 앞에 기다리는 작업이 없고, 호출한 에이전트가 `per_agent_limit` 안에 있을 때) `throtd`를 초당 한 번 확인하며, stderr에는 최대 10s마다 한 번 안내를 출력한다. 그다음 자기 자리에서 명령을 exec하므로 PID, 시그널, 종료 상태는 모두 그 명령의 것이 된다.

- **슬롯을 예약한다.** `throtd`가 lease를 발급하므로 두 `throt run` 명령이 마지막 슬롯을 동시에 차지할 수 없다. lease는 명령이 무거운 작업으로 나타나 슬롯을 넘겨받을 때, 명령이 종료될 때, 또는 60s가 지나면 끝난다. 이미 슬롯을 가진 작업 안의 `throt run`(`throt run` 아래에서 도는 make 레시피 등)은 바로 시작한다. 예약된 슬롯은 `throt status`에 나온다.
- `--timeout D`를 주면 `D`가 지난 뒤 기다림을 멈추고 그냥 실행한다(기본 5m, 0이면 무한히 기다린다). `--no-wait`는 한 번만 물어보고, 빈 슬롯이 있으면 차지하고, 어느 쪽이든 실행한다.
- **실패하면 열어 둔다.** `throtd`에 닿지 않으면 안내를 한 번 출력하고 바로 실행한다. 무엇도 일시 정지하지 않는다. `scheduler.mode`가 off이면 슬롯이 없으므로 high 압력일 때만 기다린다.
- 명령이 없으면 127로 종료한다. 실행할 수 없는 명령은 126으로 종료하고, `PATH`의 `.`을 통해서만 찾은 명령도 126이다. 셸과 달리 `throt run`은 이를 거부한다.

에이전트가 이 명령을 쓰게 하려면 `AGENTS.md`나 `CLAUDE.md`에 다음과 같은 줄을 추가한다.

```markdown
Prefix heavy commands (builds, full test suites, container builds) with `throt run --`, e.g. `throt run -- npm run build`.
```

## 설정

설정 파일은 선택 사항이다. 위치는 `~/.config/throt/config.json` 또는 `$XDG_CONFIG_HOME/throt/config.json`이고, 두 프로그램 모두 `-config PATH`로 덮어쓸 수 있다. 알 수 없는 키는 오류다. 일반적인 파일은 다음과 같다.

```json
{
  "profile": "balanced",
  "scheduler": {
    "mode": "observe",
    "per_agent_limit": 2
  }
}
```

### 프로필

`profile`은 스케줄러 한도의 프리셋이다. 명시적으로 설정한 키가 우선한다. 프로필이 스케줄러를 켜는 일은 없다.

에이전트들의 빌드가 함께 메모리를 다 쓸 수 있는 상황이라면 `enforce`(`balanced`와 함께)를 켠다. [벤치마크](docs/ARCHITECTURE.md#benchmarks)에서 이 설정은 macOS 실행 한 번에서 상한에 걸린 작업 하나를 빼고 모든 작업을 끝냈고, 기준선은 swap을 쓰다가 작업이 상한에 걸리거나 OOM killer에 죽었다. CPU만 바쁠 때는 눈에 띄는 득도 실도 없다.

| 프로필 | 무거운 작업 한도 | 적응형 |
|---|---|---|
| `performance` | CPU 수/2로 고정(최소 2). | 아니요 |
| `balanced` (기본) | CPU 수/4에서 시작해(최소 2) 머신이 버티는 동안 CPU당 하나까지 늘어난다. 그 이상은 빈 메모리에 들어가는 대기 작업에 한해 CPU당 넷까지(최대 64) 늘어난다. | 예 |
| `cool` | 1. critical 압력에서는 작업 하나를 일시 정지할 수 있다(`pause_on_critical`). | 예 |

### 설정 키

| 키 | 기본값 | 의미 |
|---|---|---|
| `scheduler.mode` | `off` | `off`: 결정하지 않는다. `observe`: 결정을 `scheduler_decision` 이벤트로 기록하고 아무것도 하지 않는다. `enforce`: 한도에 이르면 에이전트가 소유한 새 무거운 작업을 일시 정지하고, 슬롯이 비면 재개한다. |
| `profile` | `balanced` | [프로필](#프로필)을 참고한다. |
| `scheduler.heavy_limit` | 프로필에서 | 고정 한도. `adaptive`일 때는 상한(1–64). |
| `scheduler.start_limit` | 프로필에서 | 적응형 한도의 시작값(0–`heavy_limit`. 0이면 프로필 값을 쓰되 `heavy_limit`을 넘지 않는다). `adaptive`가 아니면 무시한다. |
| `scheduler.adaptive` | 프로필에서 | high 압력에서는 한도를 절반으로 줄인다. 머신이 버티는 동안 30s마다 하나씩 CPU당 하나까지 올리고, 빈 메모리에 들어가는 대기 작업이 있으면 `heavy_limit`까지 즉시 올린다. |
| `scheduler.per_agent_limit` | `0` (제한 없음) | 한 에이전트가 실행할 수 있는 무거운 작업의 최대 수. 한 에이전트가 모든 슬롯을 차지하지 못하게 한다(0–64). |
| `scheduler.background` | `true` | `enforce` 모드에서 에이전트가 소유한 무거운 작업을 모두 백그라운드 우선순위로 돌린다. macOS에서는 `taskpolicy -b`와 같은 백그라운드 상태(가장 낮은 CPU 우선순위, Apple silicon에서는 효율 코어, 디스크 I/O 제한)이고, Linux에서는 nice +10과 가장 낮은 best-effort I/O 우선순위다. `enforce`를 끄거나 이 값을 끄거나 `throtd`를 멈추면 원래대로 되돌린다. 다만 Linux에서는 우선순위를 다시 올리는 일을 root만 할 수 있어서, 보통 nice 값은 프로세스가 끝날 때까지 남는다. 프로필은 이 값을 바꾸지 않는다. |
| `scheduler.resume_cooldown` | `10s` | 압력이 high나 critical이거나 메모리, swap, latency 때문에 elevated일 때, 그리고 압력 때문에 일시 정지한 뒤에 재개(와 선점) 사이에 두는 최소 간격. 압력이 normal이거나 CPU만 원인일 때, 또는 작업이 종료되었을 때는 빈 슬롯을 즉시 채운다. 최대 `10m`. |
| `scheduler.max_queue_wait` | `5m` | 이만큼 대기한 작업은 high 압력에서도 다음 빈 슬롯을 받는다. 대기열이 기아 상태에 빠지지 않게 한다(최소 1s). |
| `scheduler.pause_on_critical` | `false` | critical 압력에서 가장 최근에 시작한 무거운 작업을 일시 정지한다. 마지막 하나는 건드리지 않는다. |
| `agents` | 없음 | 사용자 정의 에이전트: `[{"name": "myagent", "executables": ["my-agent"]}]`. 최대 32개. `name`은 소문자, 숫자, 대시로 된 1–32자이고, 각 에이전트에 실행 파일 기본 이름을 1–16개 지정한다. |
| `socket_path` | 아래 참고 | 데몬의 Unix 소켓. 재시작해야 적용된다. |
| `sample_interval` | `2s` | 지표 샘플링 주기. `100ms`–`10s`(30s 압력 윈도에 샘플이 최소 세 개 들어가야 한다). 재시작해야 적용된다. |
| `event_history` | `1000` | `throt events`용으로 `throtd`가 메모리에 보관하는 이벤트 수(1–100000). 재시작해야 적용된다. |

사용자 정의 `agents`는 내 프로세스 중에서 실행 파일 이름이 정확히 일치하는 것만 잡는다. `node` 같은 범용 런타임이나 `go` 같은 빌드 도구는 관계없는 프로세스까지 잡게 되므로 거부한다. Linux에서는 실행 파일 이름이 15자로 잘린다.

**소켓 경로.** 두 프로그램은 같은 방식으로 소켓을 찾는다. 설정의 `socket_path`를 먼저 쓰고, 없으면 `$XDG_RUNTIME_DIR/throt/throtd.sock`, 그것도 없으면(macOS에서는 보통 이 경우다) OS 임시 디렉터리(`$TMPDIR`, 없으면 `/tmp`)의 `throt-UID/throtd.sock`을 쓴다. 소켓 디렉터리는 내 소유여야 하고 다른 사용자가 쓸 수 없어야 한다. 사용 중인 경로는 `throt doctor`가 출력한다.

### 검사와 다시 불러오기

- `throt config check [--config PATH]`는 `throtd`와 똑같은 방식으로 파일을 읽어 `ok`와 기본값과 다른 값을 출력한다(각각 `file` 또는 `profile`로 표시). 파일이 잘못되었거나 없으면 오류와 함께 1로 종료한다. 실패로 처리하지는 않지만 다음 경우에는 경고한다. 고정 `heavy_limit`이 CPU 수 이상인데 `enforce`인 경우, mode가 `off`인데 `pause_on_critical`인 경우, 사용자 정의 에이전트가 기본 에이전트를 가리는 경우다.
- `throt config show [--json]`은 실제로 적용되는 설정과 각 값의 출처(`default`, `profile`, `file`)를 출력한다.
- `throt reload`(또는 SIGHUP)는 `scheduler.*`, `profile`, `agents`를 실행 중에 적용하고 바뀐 항목마다 `applied:`를 출력한다. `socket_path`, `sample_interval`, `event_history`는 `requires restart`로 보고한다. 파일을 읽지 못하면 아무것도 바꾸지 않는다. `enforce`에서 벗어나면 스케줄러가 대기열에 넣은 작업을 모두 재개하고 Throt가 낮춘 우선순위를 되돌린다. 사용자가 직접 일시 정지한 작업은 건드리지 않는다.

**로깅.** `throtd`는 stderr에 로그를 남긴다. 플래그는 `-config PATH`, `-version`, `-log-format text|json`(기본 `text`), `-log-level debug|info|warn|error`(기본 `info`)다. `info`는 데몬 생명주기, 에이전트의 등장과 종료, 압력 변화, 스케줄러 결정, 제어 동작을 다룬다. `debug`는 무거운 작업의 시작과 종료, admission lease를 하나하나 추가로 기록한다. 이벤트 로그(`throt events`)는 로그 레벨과 관계없이 모든 것을 기록한다.

## 판단 방식

- **귀속.** 프로세스는 에이전트 어댑터가 실행 파일 이름으로 정확히 일치시키는 가장 가까운 조상(또는 자기 자신)에 속한다. 그 밖의 프로세스는 귀속되지 않은 채로 남고, 귀속되지 않은 작업은 절대 제어하지 않는다. 무거운 작업(build, test, container-build)은 실행 파일 이름으로 인식하고, 빌드가 띄운 컴파일러는 그 빌드에 합친다. OS에서 읽는 것은 실행 파일 이름뿐이다. 인자나 환경 변수는 읽지 않는다. `throtd`가 받는 명령 텍스트는 훅과 `throt run`이 보내는 [시그니처](#학습한-무거운-명령)뿐이다.
- **압력.** 메모리, swap 증가량, CPU, load, 포그라운드 latency(10ms 타이머가 얼마나 늦게 울리는지)의 윈도 중앙값과, 커널 자체 신호가 있으면 그 신호(macOS의 memory-pressure 판정, Linux의 PSI)를 합쳐 `normal`, `elevated`, `high`, `critical` 중 하나를 정한다. 순간적인 튐은 무시하도록 히스테리시스를 둔다. CPU가 바쁜 것만으로는 elevated를 넘지 않는다. 포화된 코어는 처리량이고, 그것이 사용자에게 주는 영향은 latency 프로브가 잰다.
- **스케줄링**(`scheduler.mode`를 설정했을 때만). Throt는 이미 존재하는 프로세스에만 손을 쓸 수 있다. 그래서 모든 슬롯이 찬 상태에서 새 무거운 작업이 나타나면 샘플 주기 한 번 안에 일시 정지하고, 슬롯이 나면 재개한다. 이미 실행 중인 작업은 대기열에 넣지 않는다. high 압력이면 새 시작을 막는다. 대기 중인 작업과 `throt run` 호출자는 선착순 줄 하나를 함께 쓴다. 모든 결정은 reason 코드가 붙은 이벤트로 남는다.
- **제어.** 일시 정지와 재개는 내 에이전트의 무거운 작업에만 적용한다. 시그널은 프로세스의 신원(PID, 시작 시각, uid)을 확인한 뒤에만 보낸다. 일시 정지한 프로세스는 정지하기 전에 `paused.json`에 기록하므로 재시작한 `throtd`가 이를 재개한다.

스케줄러는 기본적으로 꺼져 있다. 벤치마크에서는 빌드들이 함께 메모리를 다 쓸 때 효과가 있었고, CPU만 바쁠 때는 눈에 띄는 차이가 없었다. 그래도 에이전트의 작업을 멈추는 일이니 켤지는 사용자가 정한다([프로필](#프로필) 참고). 세부 사항, 벤치마크 결과, 로컬 프로토콜은 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)에 있다. [Scheduler](docs/ARCHITECTURE.md#scheduler), [Control](docs/ARCHITECTURE.md#control), [Results](docs/ARCHITECTURE.md#results)를 참고한다.

## 알려진 제약

- **귀속은 실행 파일 이름과 조상 관계로 판단한다.** `node script.js`나 `python3 -m …`로 시작한 에이전트는 인식하지 못한다. 고유한 이름을 가진 래퍼를 만들고 [사용자 정의 규칙](#에이전트가-감지되지-않는다)을 추가한다. 이름은 인증하지 않으므로 `claude`라는 이름의 프로그램은 무엇이든 에이전트로 친다.
- **무거운 작업은 이름이나 실제 사용량으로 인식한다.** 에이전트가 셸로 실행한 명령은 이름과 상관없이 바쁘면(1코어 이상 또는 1 GiB 이상) 무거운 작업으로 친다. 다만 명령이 시작되기 전에 기다리게 하는 `throt run`과 Claude Code hook은 이름이나, 이전 실행에서 [학습한 시그니처](#학습한-무거운-명령)로 판단한다. 그래서 처음 보는 명령이 처음 무겁게 실행될 때는 시작 전에 붙잡지 못한다. `npm`, `pnpm`, `yarn`, `bun`, `java`, `dotnet`은 바쁠 때만 무거운 작업으로 친다. 하지만 빌드 도구 이름을 가진 다른 오래 사는 프로세스(`next dev`, `vite`)는 실행되는 내내 무거운 작업 슬롯을 차지한다.
- **Throt는 이미 존재하는 프로세스에만 손을 쓴다.** 새 빌드가 한꺼번에 몰리면 초과분이 일시 정지되기 전까지 최대 샘플 주기 한 번(기본 2초) 동안 실행된다. 주기 한 번보다 짧게 끝나는 프로세스는 아예 보이지 않는다.
- **일시 정지해도 메모리는 풀리지 않는다.** 일시 정지한 빌드는 할당한 메모리를 그대로 쥐고 있다. 커널이 swap으로 내보낼 수는 있지만, swap이 적거나 없는 컨테이너에서는 시작 직후 일시 정지한 작업이 OOM으로 죽을 수 있다. 메모리를 비워 두는 방법은 시작 전 승인(Claude Code 훅, `throt run --`)뿐이다. [Gradle, Maven, Docker](#gradle-maven-docker)를 참고한다.
- **빌드 데몬과 컨테이너는 귀속 대상에서 보이지 않는다.** Gradle이나 Kotlin 데몬은 어떤 에이전트에도 속하지 않는 분리된 `java` 프로세스라서, 그 안에서 도는 빌드는 대기열에 들어가지 않는다(`org.gradle.daemon=false`로 설정하고, Kotlin은 `kotlin.compiler.execution.strategy=in-process`도 설정한다). 컨테이너는 Testcontainers가 띄운 것까지 포함해 Docker VM 안(macOS)이나 `containerd` 아래에서 돌아 모든 에이전트의 프로세스 트리 밖에 있다. 막을 수 있는 건 컨테이너를 띄우는 명령뿐이다.
- **시그널은 확인한 뒤에 보낸다.** macOS와 pidfd가 없는 Linux에서는 이론상 신원 확인과 시그널 사이에 PID가 재사용될 수 있다. [SECURITY.md](SECURITY.md)를 참고한다.
- **lease와 통계는 메모리에만 있다.** 재시작한 `throtd`는 `throt run` 예약과 `throt stats`를 잊는다(일시 정지한 작업은 디스크에 기록되어 재개된다).
- **플랫폼.** macOS와 Linux만 지원한다. Linux에서는 cgroup v1과 `cpuset` 한도를 읽지 않고, PSI 외에는 커널 메모리 판정이 없다.

## 문제 해결

먼저 `throt doctor`를 실행한다. 소켓 경로, 사용 중인 설정, `throtd` 연결 여부, 누락된 지표, 정지된 프로세스를 보여 주며, 그대로 공유해도 안전하다.

### "cannot reach throtd"

**증상.** TUI에 `cannot reach throtd at PATH: start it with throtd`가 표시되거나, 명령이 `connect to throtd at PATH (is it running?): ...`로 실패한다.

**원인.** `throtd`가 실행 중이 아니거나 다른 소켓에서 대기하고 있다. 소켓 경로는 `socket_path`, `XDG_RUNTIME_DIR`, `TMPDIR`에 따라 달라진다. 그래서 서비스 관리자, `sudo`, SSH, cron에서 시작한 `throtd`는 셸과 다른 경로를 쓸 수 있다.

**해결.**
- `... socket dir: lstat ...: no such file or directory` 또는 `connection refused`: `throtd`를 시작한다.
- 실행 중이라면 오류에 나온 경로(또는 `throt doctor`의 `socket_path`)를 `throtd`가 시작할 때 기록한 경로(`listening on PATH`, 첫 `daemon_started` 이벤트에도 있다)와 비교한다. 양쪽에 같은 환경을 주거나, 양쪽이 읽는 설정에 같은 절대 경로 `socket_path`를 지정한다.
- `socket dir PATH is owned by uid N, not the current user`, `is writable by other users (mode ...)`, `is not a directory (symlinks are not allowed)`: 소켓 디렉터리가 비공개 디렉터리 검사를 통과하지 못했다. 이 검사는 다른 사용자로부터 소켓을 보호한다. 디렉터리가 내 것이라면 `chmod 700`을 하거나 디렉터리를 지우고 `throtd`를 재시작한다. 그렇지 않다면 나만 쓸 수 있는 디렉터리로 `socket_path`를 지정한다.
- 시작할 때 `throtd is already running on PATH`: 다른 `throtd`가 소켓을 쥐고 있다. 그 데몬을 쓰거나 먼저 멈춘다.

### 업데이트 후 프로토콜 불일치

**증상.** 명령이 `protocol mismatch: client speaks vX, daemon speaks vY; upgrade throt and throtd to the same release`로 실패하거나, `throt version --json`의 `daemon.version`이 자신의 `version`과 다르다.

**원인.** `throt update`는 바이너리를 교체하지만 실행 중인 `throtd`를 재시작하지는 않는다. 데몬은 재시작할 때까지 옛 버전으로 남는다.

**해결.** `throt update`가 출력한 명령으로 `throtd`를 재시작한다(`launchctl kickstart -k gui/$(id -u)/dev.throt.throtd`, `systemctl --user restart throtd`, 또는 멈췄다가 다시 시작). 멈추면 그동안 일시 정지한 작업을 모두 재개한다. 그래도 오류가 계속되면 `PATH`에 있는 `throt`와 `throtd`가 서로 다른 릴리스에서 온 것이다. 한 아카이브에서 둘 다 다시 설치한다.

### 프로세스가 정지 상태(`T`)에 멈춰 있다

**증상.** `throt status`가 `STOPPED   N processes not paused by throtd (see throt doctor)`를 출력하거나, `ps`에서 빌드가 `T` 상태로 보인다.

**원인.** 프로세스가 정지(SIGSTOP)되었는데 이를 설명하는 일시 정지 기록이 없다. `paused.json`을 쓰기 전에 강제 종료된 `throtd`, 삭제된 상태 파일, 떠도는 데몬, Ctrl-Z로 중단한 셸 작업 등이 원인이다. Throt는 이런 프로세스를 보고만 하고 스스로 재개하지 않는다. (`throtd` 자신이 크래시 전에 일시 정지한 작업은 `throtd`가 다시 시작할 때 재개된다.)

**해결.**
- `throt doctor`는 이런 프로세스를 `orphans` 아래에 나열한다(최대 50개: PID, 실행 파일 이름, 소유 에이전트, 정지된 시간, 힌트). `throtd`가 꺼져 있어도 동작한다.
- 힌트가 `likely left paused by an earlier throtd`이면 힌트에 나온 명령을 실행한다. `throt resume PID`는 무거운 작업의 정지된 프로세스를 자식부터 재개한다. 에이전트의 다른 프로세스에는 `--force`가 필요하다.
- 힌트가 `likely job control (Ctrl-Z)`이면 해당 셸에서 `fg`를 실행한다. 이 경우 `throt resume PID`는 `likely a shell job stopped with Ctrl-Z; use fg in its shell, or throt resume --force`라며 거부한다.
- 그 밖의 경우: `throt resume --force PID`는 내 소유의 정지된 프로세스를 무엇이든 재개한다(정지를 강제하는 일은 없다). `kill -CONT PID`는 언제나 통한다.

### 에이전트가 감지되지 않는다

**증상.** `throt status`나 TUI의 `AGENTS`에 에이전트가 없고, 그 에이전트의 빌드에 소유자가 표시되지 않는다.

**원인.** 에이전트는 내 프로세스 중에서 실행 파일 이름이 정확히 일치하는 것만 잡는다. 기본 지원: `claude`, `codex`, `opencode`, `gemini`, `aider`, `cursor-agent`, `goose`, `amp`, `copilot`, `orca`. 인자는 읽지 않으므로 다른 이름으로 시작했거나 `node script.js`, `python3 -m aider`처럼 시작한 에이전트는 어디에도 일치하지 않아 귀속되지 않는다.

**해결.** 그 프로세스의 실제 실행 파일 이름(`ps -o comm= -p PID`의 마지막 경로 요소. Linux에서는 최대 15자)으로 사용자 정의 에이전트를 추가한 뒤 `throt config check`와 `throt reload`를 실행한다.

```json
{"agents": [{"name": "myagent", "executables": ["my-agent"]}]}
```

범용 런타임과 빌드 도구는 거부한다. 예를 들어 `agents.myagent: "node" is a generic runtime and would match unrelated processes`나 `... is a build tool, not an agent`가 나온다. 관계없는 프로세스까지 가져가기 때문이다. 대신 고유한 이름을 가진 래퍼나 바이너리로 에이전트를 실행한다.

### macOS에서 압력이 항상 elevated로 나온다

**증상.** `throt status`에 `PRESSURE  ELEVATED (kernel_memory_pressure>=warning)`와 `MEM ... (kernel: warning)`이 나오고, TUI에는 `why: kernel memory pressure warning`이 나온다.

**원인.** macOS는 일부러 메모리를 가득 채워 두기 때문에 Throt는 메모리 사용률 대신 커널 자체의 memory-pressure 판정(`sysctl kern.memorystatus_vm_pressure_level`: 1 normal, 2 warning, 4 critical)을 쓴다. `warning`은 커널 스스로 메모리가 빠듯하다고 본다는 뜻이다(압축과 회수를 하는 중이다). 그래서 Throt는 elevated로 보고한다. `critical`은 high에 대응한다.

**해결.** 대개는 할 일이 없다. 정확한 측정값이고, elevated는 새 시작을 막지 않는다(막는 것은 high뿐이다). 다만 적응형 한도가 늘어나지 못하게 한다. 낮추려면 메모리를 확보한다(쓰지 않는 앱을 종료하고, 쓰지 않는 컨테이너나 에이전트를 멈춘다). 다른 이유는 다른 뜻이다. `cpu>=0.85`나 `load>=1`은 CPU가 바쁘다는 뜻으로, elevated를 넘지 않고 한도를 붙잡지도 않는다. macOS에서 `memory_used>=0.8`은 판정을 읽지 못했다는 뜻이다(`throt doctor`가 `host_metrics.missing` 아래에 `memory_pressure`를 표시한다).

### macOS가 throt나 throtd를 열지 않는다

**증상.** 브라우저로 릴리스를 내려받은 뒤 macOS가 개발자를 확인할 수 없다는 대화 상자를 띄우며 `throt`나 `throtd`를 막는다.

**원인.** Gatekeeper 때문이다. 바이너리는 서명도 공증도 받지 않았고, 브라우저로 내려받은 파일에는 `com.apple.quarantine` 속성이 붙는다.

**해결.** `xattr -d com.apple.quarantine throt throtd`를 한 번 실행한다. `gh release download`나 `curl`로 받거나 `throt update`를 쓰면 이 속성이 붙지 않는다.

### `throt update`가 404로 실패한다

**증상.** `GitHub returned 404 for cfcromn/Throt: the repository is private, so the request needs a token` 또는 `... the token in use cannot read it`.

**원인.** GitHub는 보여 줄 수 없는 비공개 저장소에 404로 응답한다. `throt update`가 토큰을 찾지 못했거나, 찾은 토큰에 접근 권한이 없다.

**해결.** 저장소를 읽을 수 있는 토큰을 `GH_TOKEN`(또는 `GITHUB_TOKEN`)에 설정하거나, GitHub CLI를 설치하고 `gh auth login`을 실행한다. 그러면 `throt update`가 `gh auth token`을 쓴다. `GitHub rejected the token (401)`은 토큰 자체가 잘못되었거나 만료되었다는 뜻이다. `gh auth status`로 확인한다.

### Claude Code 훅 때문에 명령 실행이 늦어진다

**증상.** Claude Code에서 Bash 명령이 시작되기까지 한참 걸리고, 그 뒤 `throt: waited 40s for a heavy-job slot` 또는 `throt: no heavy-job slot after 110s (REASON); running anyway`가 나온다.

**원인.** 명령이 무거운 도구 하나뿐이거나 [학습한 무거운 명령](#학습한-무거운-명령)이고(이때는 메시지에 `learned heavy`가 붙는다) 용량 판정이 '안 된다'이면 훅은 최대 `--max-wait`만큼 기다린다. 압력이 high이거나, 무거운 작업 슬롯이 모두 쓰이고 있거나, 이미 대기 중인 작업이 있거나, 에이전트가 `per_agent_limit`에 걸린 경우다. 메시지의 이유가 그중 무엇인지 알려 준다.

**해결.**
- `throt status`로 압력과 슬롯을 확인한다. `scheduler.mode`가 off이면 high 압력일 때만 기다린다.
- 학습한 명령인데 기다리지 않았으면 할 때는 `throt learned --forget 'SIGNATURE'`를 쓴다(다시 무거운 작업을 띄우면 다시 학습한다).
- 훅 명령의 대기 시간을 줄인다. 예를 들어 `throt hook claude-code --max-wait 30s`로 하되 훅 `timeout`보다 짧게 유지한다. 전혀 기다리지 않으려면 `--max-wait 0`을 쓴다.
- 가벼운 호출은 기다리지 않는다. 첫 인자가 `-v`, `--version`, `version`, `-h`, `--help`, `help`, `-n`, `--dry-run`인 경우와 `go env`, `go list`, `go mod`, `npm view`, `npm ls`, `cargo metadata`, `gradle tasks`(`./gradlew tasks`, `dependencies`, `--stop`, `--status`도) 같은 읽기 전용 하위 명령이 그렇다. 그 밖의 읽기 전용 하위 명령은 여전히 기다리고, Docker 이미지 빌드도 기다린다. 다른 `docker` 명령은 `docker compose up`을 포함해 기다리지 않는다.

## 로드맵

날짜가 아니라 결과로 정한다.

| 버전 | 목표 | 주요 내용 | 상태 |
|---|---|---|---|
| v0.1 Observe | "무엇이 내 머신을 느리게 하고, 어느 에이전트의 것인가?" | 호스트 지표, 프로세스 그래프, 에이전트 귀속, TUI, 이벤트 타임라인, `throt doctor` | 출시 |
| v0.2 Control | 작업을 잃지 않고 겹치는 무거운 작업 줄이기 | 수동 일시 정지/재개, 무거운 작업 슬롯 스케줄러(옵트인), 감사 이벤트, 크래시 복구 | 출시 |
| v0.3 Adaptive | 머신의 반응성을 자동으로 유지하기 | latency 신호, 적응형 한도, 에이전트별 예산, 프로필 | 출시 |
| v0.4 Ecosystem | 어디에나 통합하기 | Linux, 문서화된 로컬 API, MCP 서버, 사용자 정의 에이전트 어댑터 | 출시 |
| v0.5 Accurate pressure | 어디서나 정확한 압력 측정과 견고화 | cgroup 인식 Linux, macOS 커널 판정, 근거 기반 CPU 정책, 핫 리로드, `throt explain`, 퍼징, 고정된 공급망 | 출시 |
| v0.6 Cooperative admission | 일시 정지당하는 대신 에이전트가 기다리게 하기 | `throt run`, `throt stats`, Linux PSI, TUI 상세 화면과 스파크라인, 실제 바이너리 e2e 테스트 | 출시 |
| v0.7 Admission leases | 경쟁 없는 admission, 숨은 정지 작업 없애기 | `throt run`용 admission lease, Throt가 일시 정지하지 않은 정지 프로세스 보고 | 출시 |
| v0.8 Integration and readiness | 지시 없이도 용량을 기다리기 | Claude Code 훅, `throt setup`, `throt config check`/`show`, `throt version --json`, 로그 레벨, soak 테스트 | 출시 (v0.8.0-rc.1) |
| v0.9 Evidence and upkeep | 실제 머신에서 측정하고 최신 상태 유지하기 | `throt update`, 실제 머신 CPU·메모리 벤치마크, CI 러너에서 soak, CPU만 바쁠 때 늘어나는 적응형 한도, `start_limit`, `throt_wait_for_capacity`와 `throt_explain`을 갖춘 MCP `2026-07-28`, TUI 스케줄러 줄과 이벤트 필터 | 출시 (v0.9.0-rc.1) |
| 1.0 | 명시한 프로필 아래에서 자신 있게 스케줄러 켜기 | 안정된 호환성 정책, 실제 머신 벤치마크. 벤치마크 근거에 따라 스케줄러는 옵트인으로 남긴다 | 출시 (v1.0.0-rc.1) |

**1.0**은 명시한 `scheduler.mode`와 `profile` 아래에서 스케줄러를 자신 있게 켤 수 있다는 뜻이다. 기본값은 여전히 꺼짐이다([docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#results)의 "Default decision" 참고). 1.0의 조건은 지원 OS 계열 두 개, [호환성 정책](docs/ARCHITECTURE.md#compatibility) 시행, 알려진 프로세스 유실 버그 없음, 재현 가능한 벤치마크, IPC와 프로세스 제어 경계에 대한 보안 검토다.

## 기여와 라이선스

- [기여와 개발](CONTRIBUTING.md): 빌드, 테스트, 프로세스 제어 변경 규칙.
- [보안과 개인정보](SECURITY.md): 취약점 신고, 보안 검토, Throt가 수집하는 정보.
- [아키텍처와 설계](docs/ARCHITECTURE.md)
- [변경 내역](CHANGELOG.md)

Throt는 [MIT License](LICENSE)로 배포된다.

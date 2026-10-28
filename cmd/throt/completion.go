package main

import (
	"fmt"
	"io"
)

// The completion scripts complete commands and flags. After pause, resume
// and explain they offer the heavy-workload PIDs that `throt status` lists,
// so completing there asks throtd (nothing is offered when it is down).

const bashCompletion = `# bash completion for throt. Load with: source <(throt completion bash)
_throt_pids() {
    throt status 2>/dev/null | awk '/^HEAVY WORKLOADS/ {f = 1; next} f && $2 ~ /^[0-9]+$/ {print $2}'
}

_throt() {
    local cur prev i cmd=""
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    for ((i = 1; i < COMP_CWORD; i++)); do
        case "${COMP_WORDS[i]}" in
            -config|--config|-n|--n) ((i++)) ;;
            -*) ;;
            *) cmd="${COMP_WORDS[i]}"; break ;;
        esac
    done
    if [[ -z "$cmd" ]]; then
        case "$prev" in
            -config|--config) COMPREPLY=($(compgen -f -- "$cur")); return ;;
            -n|--n) return ;;
        esac
        COMPREPLY=($(compgen -W "top status stats ping events explain pause resume run reload config doctor mcp completion version -config -n" -- "$cur"))
        return
    fi
    local pos=$((COMP_CWORD - i))
    case "$cmd" in
        events) COMPREPLY=($(compgen -W "-f -n" -- "$cur")) ;;
        pause|resume|explain) [[ $pos -eq 1 ]] && COMPREPLY=($(compgen -W "$(_throt_pids)" -- "$cur")) ;;
        completion) [[ $pos -eq 1 ]] && COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur")) ;;
        config) [[ $pos -eq 1 ]] && COMPREPLY=($(compgen -W "check show" -- "$cur")) || COMPREPLY=($(compgen -W "--config --json" -- "$cur")) ;;
        version) COMPREPLY=($(compgen -W "--json" -- "$cur")) ;;
        run)
            local j
            for ((j = i + 1; j < COMP_CWORD; j++)); do
                case "${COMP_WORDS[j]}" in
                    --timeout|-timeout) ((j++)) ;;
                    -*) [[ "${COMP_WORDS[j]}" == "--" ]] && { ((j++)); break; } ;;
                    *) break ;;
                esac
            done
            if [[ $j -ge $COMP_CWORD ]]; then
                case "$prev" in --timeout|-timeout) return ;; esac
                if [[ "$cur" == -* ]]; then
                    COMPREPLY=($(compgen -W "--timeout --no-wait --" -- "$cur"))
                else
                    COMPREPLY=($(compgen -c -- "$cur"))
                fi
            fi
            ;;
    esac
}

complete -o default -F _throt throt
`

const zshCompletion = `#compdef throt
# zsh completion for throt. Load with: source <(throt completion zsh)

_throt_pids() {
    local -a pids
    pids=(${(f)"$(throt status 2>/dev/null | awk '/^HEAVY WORKLOADS/ {f = 1; next} f && $2 ~ /^[0-9]+$/ {print $2}')"})
    compadd -a pids
}

_throt() {
    local -a commands
    commands=(
        'top:open the terminal UI'
        'status:show host resources and pressure'
        'ping:check that throtd is reachable'
        'events:print recent daemon events'
        'explain:explain why a heavy workload is running, paused, or queued'
        'pause:pause a heavy workload'
        'resume:resume a paused workload'
        'run:wait for heavy-job capacity, then run a command'
        'stats:per-agent resource usage since throtd started'
        'reload:make throtd re-read its config file'
        'config:check or show the config file'
        'doctor:print a sanitized diagnostic report'
        'mcp:run a read-only MCP server on stdio'
        'completion:print a shell completion script'
        'version:print the client version'
    )
    local curcontext="$curcontext" state line
    _arguments -C \
        '-config[path to config file]:file:_files' \
        '-n[number of events]:count:' \
        '1:command:->command' \
        '*::arg:->args'
    case $state in
        command) _describe -t commands 'throt command' commands ;;
        args)
            case $words[1] in
                events) _arguments '-f[keep printing new events]' '-n[number of recent events]:count:' ;;
                pause|resume|explain) (( CURRENT == 2 )) && _throt_pids ;;
                completion) (( CURRENT == 2 )) && compadd bash zsh fish ;;
                config) (( CURRENT == 2 )) && compadd check show || _arguments '--config[path to config file]:file:_files' '--json[print JSON]' ;;
                version) _arguments '--json[print JSON]' ;;
                run) _arguments -S '--timeout[run anyway after this long]:duration:' '--no-wait[check once and run regardless]' '*::command:_normal' ;;
            esac
            ;;
    esac
}

if [ "$funcstack[1]" = "_throt" ]; then
    _throt "$@"
else
    compdef _throt throt
fi
`

const fishCompletion = `# fish completion for throt. Load with: throt completion fish | source
function __throt_pids
    throt status 2>/dev/null | awk '/^HEAVY WORKLOADS/ {f = 1; next} f && $2 ~ /^[0-9]+$/ {print $2}'
end

complete -c throt -f
complete -c throt -n __fish_use_subcommand -o config -r -F -d 'path to config file'
complete -c throt -n __fish_use_subcommand -o n -x -d 'number of events'
complete -c throt -n __fish_use_subcommand -a top -d 'open the terminal UI'
complete -c throt -n __fish_use_subcommand -a status -d 'show host resources and pressure'
complete -c throt -n __fish_use_subcommand -a ping -d 'check that throtd is reachable'
complete -c throt -n __fish_use_subcommand -a events -d 'print recent daemon events'
complete -c throt -n __fish_use_subcommand -a explain -d 'explain why a heavy workload is running, paused, or queued'
complete -c throt -n __fish_use_subcommand -a pause -d 'pause a heavy workload'
complete -c throt -n __fish_use_subcommand -a resume -d 'resume a paused workload'
complete -c throt -n __fish_use_subcommand -a run -d 'wait for heavy-job capacity, then run a command'
complete -c throt -n __fish_use_subcommand -a stats -d 'per-agent resource usage since throtd started'
complete -c throt -n __fish_use_subcommand -a reload -d 'make throtd re-read its config file'
complete -c throt -n __fish_use_subcommand -a config -d 'check or show the config file'
complete -c throt -n __fish_use_subcommand -a doctor -d 'print a sanitized diagnostic report'
complete -c throt -n __fish_use_subcommand -a mcp -d 'run a read-only MCP server on stdio'
complete -c throt -n __fish_use_subcommand -a completion -d 'print a shell completion script'
complete -c throt -n __fish_use_subcommand -a version -d 'print the client version'
complete -c throt -n '__fish_seen_subcommand_from events' -o f -d 'keep printing new events'
complete -c throt -n '__fish_seen_subcommand_from events' -o n -x -d 'number of recent events'
complete -c throt -n '__fish_seen_subcommand_from pause resume explain' -x -a '(__throt_pids)'
complete -c throt -n '__fish_seen_subcommand_from completion' -x -a 'bash zsh fish'
complete -c throt -n '__fish_seen_subcommand_from config; and not __fish_seen_subcommand_from check show' -x -a 'check show'
complete -c throt -n '__fish_seen_subcommand_from config' -l config -r -F -d 'path to config file'
complete -c throt -n '__fish_seen_subcommand_from config version' -l json -d 'print JSON'
complete -c throt -n '__fish_seen_subcommand_from run' -l timeout -x -d 'run anyway after this long'
complete -c throt -n '__fish_seen_subcommand_from run' -l no-wait -d 'check once and run regardless'
complete -c throt -n '__fish_seen_subcommand_from run' -a '(__fish_complete_command)'
`

// printCompletion writes the completion script for shell.
func printCompletion(w io.Writer, shell string) error {
	var script string
	switch shell {
	case "bash":
		script = bashCompletion
	case "zsh":
		script = zshCompletion
	case "fish":
		script = fishCompletion
	default:
		return fmt.Errorf("usage: throt completion bash|zsh|fish")
	}
	_, err := io.WriteString(w, script)
	return err
}

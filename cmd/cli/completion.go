// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"fmt"
	"io"
)

func completion(shell string, out, stderr io.Writer) int {
	switch shell {
	case "bash":
		fmt.Fprint(out, `# source <(cli completion bash)
_cli_complete() {
 local cur="${COMP_WORDS[COMP_CWORD]}"
 local words="help version contexts login logout whoami clusters operations backups hosts services vuln plan completion"
 case "${COMP_WORDS[1]}" in
 clusters) words="list get create plan delete approve" ;;
 operations) words="list watch abort approve" ;;
 backups) words="list run verify" ;;
 hosts) words="list" ;;
 services) words="plan create" ;;
 vuln) words="findings approve" ;;
 plan) words="export" ;;
 contexts) words="list use set" ;;
 esac
 COMPREPLY=( $(compgen -W "$words" -- "$cur") )
}
complete -F _cli_complete cli
`)
	case "zsh":
		fmt.Fprint(out, `#compdef cli
_cli_complete() {
 local -a commands
 commands=(help version contexts login logout whoami clusters operations backups hosts services vuln plan completion)
 if (( CURRENT == 2 )); then _describe 'command' commands; return; fi
 case "$words[2]" in
 clusters) commands=(list get create plan delete approve) ;;
 operations) commands=(list watch abort approve) ;;
 backups) commands=(list run verify) ;;
 hosts) commands=(list) ;;
 services) commands=(plan create) ;;
 vuln) commands=(findings approve) ;;
 plan) commands=(export) ;;
 contexts) commands=(list use set) ;;
 esac
 _describe 'subcommand' commands
}
compdef _cli_complete cli
`)
	case "fish":
		fmt.Fprint(out, `complete -c cli -f
complete -c cli -n '__fish_use_subcommand' -a 'help version contexts login logout whoami clusters operations backups hosts services vuln plan completion'
complete -c cli -n '__fish_seen_subcommand_from contexts' -a 'list use set'
complete -c cli -n '__fish_seen_subcommand_from clusters' -a 'list get create plan delete approve'
complete -c cli -n '__fish_seen_subcommand_from operations' -a 'list watch abort approve'
complete -c cli -n '__fish_seen_subcommand_from backups' -a 'list run verify'
complete -c cli -n '__fish_seen_subcommand_from hosts' -a 'list'
complete -c cli -n '__fish_seen_subcommand_from services' -a 'plan create'
complete -c cli -n '__fish_seen_subcommand_from vuln' -a 'findings approve'
complete -c cli -n '__fish_seen_subcommand_from plan' -a 'export'
`)
	default:
		fmt.Fprintln(stderr, "supported shells: bash, zsh, fish")
		return 2
	}
	return 0
}

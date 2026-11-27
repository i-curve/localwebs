package main

import (
	"fmt"
)

func completion(shell string) error {
	switch shell {
	case "bash":
		fmt.Print(`_localwebs_cli() {
  local words="interactive receive send peers completion -server -code -dir -to -text -stdin -clipboard -image"
  if [[ ${COMP_WORDS[1]} == completion ]]; then words="bash zsh fish powershell"; fi
  COMPREPLY=( $(compgen -W "$words" -- "${COMP_WORDS[COMP_CWORD]}") )
  if [[ ${#COMPREPLY[@]} -eq 0 ]]; then COMPREPLY=( $(compgen -f -- "${COMP_WORDS[COMP_CWORD]}") ); fi
}
complete -F _localwebs_cli localwebs-cli
`)
	case "zsh":
		fmt.Print(`_localwebs_cli() {
  _alternative 'commands:command:compadd interactive receive send peers completion bash zsh fish powershell -server -code -dir -to -text -stdin -clipboard -image' 'files:file:_files'
}
compdef _localwebs_cli localwebs-cli
`)
	case "fish":
		fmt.Print(`complete -c localwebs-cli -a 'interactive receive send peers completion'
complete -c localwebs-cli -n '__fish_seen_subcommand_from completion' -a 'bash zsh fish powershell'
complete -c localwebs-cli -o server -r
complete -c localwebs-cli -o code -r
complete -c localwebs-cli -o dir -r
complete -c localwebs-cli -o to -r
complete -c localwebs-cli -o text -r
complete -c localwebs-cli -o stdin
complete -c localwebs-cli -o clipboard
complete -c localwebs-cli -o image
`)
	case "powershell":
		fmt.Print(`Register-ArgumentCompleter -Native -CommandName localwebs-cli -ScriptBlock {
  param($wordToComplete, $commandAst, $cursorPosition)
  'interactive','receive','send','peers','completion','bash','zsh','fish','powershell','-server','-code','-dir','-to','-text','-stdin','-clipboard','-image' |
    Where-Object { $_ -like "$wordToComplete*" } |
    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
}
`)
	default: return fmt.Errorf("unsupported shell %q", shell)
	}
	return nil
}

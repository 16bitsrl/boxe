package main

import (
	"fmt"
	"strings"

	"github.com/gliderlabs/ssh"
	"golang.org/x/term"
)

// commands are what tab completes first at the prompt.
var commands = []string{"new", "ls", "rm", "cp", "fork", "ttl", "restart", "sleep", "wake", "keep-awake",
	"share", "unshare", "exec", "images", "token", "ssh-config", "known-hosts", "whoami", "help", "exit"}

// prompt is `ssh boxe` from a terminal: one command after another, with
// history and tab completion, until exit or ctrl-d.
func (b *Box) prompt(s ssh.Session, user string, size ssh.Window, resized <-chan ssh.Window) {
	t := term.NewTerminal(s, b.name+" ▶ ")
	t.SetSize(size.Width, size.Height)
	go func() {
		for w := range resized {
			t.SetSize(w.Width, w.Height)
		}
	}()
	t.AutoCompleteCallback = func(line string, pos int, key rune) (string, int, bool) {
		if key != '\t' {
			return "", 0, false
		}
		return b.complete(user, line, pos)
	}
	fmt.Fprintln(t, "boxe: microVMs for your team. Type help for the commands, exit to leave.")
	for {
		line, err := t.ReadLine()
		if err != nil {
			return
		}
		switch strings.TrimSpace(line) {
		case "":
			continue
		case "exit", "quit":
			return
		}
		if err := b.run(t, user, line, ""); err != nil {
			fmt.Fprintln(t, "error:", err)
		}
	}
}

// complete fills in the word before the cursor: a command first, then one
// of the user's VMs. With several matches it goes as far as they agree.
func (b *Box) complete(user, line string, pos int) (string, int, bool) {
	r := []rune(line)
	before := string(r[:pos])
	start := strings.LastIndexByte(before, ' ') + 1
	word := before[start:]
	var options []string
	if strings.TrimSpace(before[:start]) == "" {
		options = commands
	} else {
		vms, err := b.store.List(user)
		if err != nil {
			return "", 0, false
		}
		for _, v := range vms {
			options = append(options, v.Name)
		}
	}
	var match []string
	for _, o := range options {
		if strings.HasPrefix(o, word) {
			match = append(match, o)
		}
	}
	if len(match) == 0 {
		return "", 0, false
	}
	common := match[0]
	for _, m := range match[1:] {
		for !strings.HasPrefix(m, common) {
			common = common[:len(common)-1]
		}
	}
	if len(match) == 1 {
		common += " "
	}
	if common == word {
		return "", 0, false
	}
	head := before[:start] + common
	return head + string(r[pos:]), len([]rune(head)), true
}

package monitor

import (
	"encoding/json"
	"regexp"
	"strings"
)

// keyMaterial is text that is a secret by its shape: a private key block,
// or a token in a format its issuer documents.
var keyMaterial = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----|\bAKIA[0-9A-Z]{16}\b|\bASIA[0-9A-Z]{16}\b|` +
	`\bgh[pousr]_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{40,}\b|\bglpat-[A-Za-z0-9_-]{20,}\b|\bxox[abprs]-[A-Za-z0-9-]{10,}\b|` +
	`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}\b|\bAIza[0-9A-Za-z_-]{35}\b|(?i)\baws_secret_access_key\s*[=:]\s*\S{20,}|` +
	`(?im)^\s*(?:export\s+)?[A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_KEY|PRIVATE_KEY)[A-Z0-9_]*\s*=\s*\S{8,}`)

// printers write a file's content to their output.
var printers = map[string]bool{"cat": true, "head": true, "tail": true, "less": true, "more": true, "base64": true,
	"xxd": true, "od": true, "hexdump": true, "strings": true, "grep": true, "egrep": true, "fgrep": true, "awk": true,
	"sed": true, "nl": true, "tac": true, "cut": true, "sort": true, "uniq": true, "tr": true, "jq": true, "yq": true,
	"bat": true, "type": true, "gpg": true}

// CredentialSeen reports whether a call that ran brought credential content
// into the session: its output carries key material, or it read a
// credential file and returned what it read. A command that only names a key
// file, as a copy or a check does, and prints none of it, is not one.
func CredentialSeen(tool, args, output string) bool {
	if strings.TrimSpace(output) == "" {
		return false
	}
	if keyMaterial.MatchString(output) {
		return true
	}
	var a map[string]any
	_ = json.Unmarshal([]byte(args), &a)
	str := func(k string) string { s, _ := a[k].(string); return s }
	switch tool {
	case "read", "grep":
		return credentialish(" " + str("path"))
	case "bash", "ssh":
		s, ok := parseScript(str("command"))
		if !ok {
			return false
		}
		for _, cmd := range s.cmds {
			prog, rest := cmd.program()
			for _, r := range cmd.reads {
				if printers[prog] && credentialish(" "+r) {
					return true
				}
			}
			if prints(prog, rest) {
				for _, w := range operands(rest) {
					if credentialish(" " + w) {
						return true
					}
				}
				for i, w := range rest {
					if (w == "-in" || w == "--in") && i+1 < len(rest) && credentialish(" "+rest[i+1]) {
						return true
					}
				}
			}
		}
	}
	return false
}

// prints reports a command that writes the files it is given to its output.
func prints(prog string, args []string) bool {
	switch {
	case prog == "sed":
		return !hasFlag(args, "-i") && !hasPrefix(args, "-i")
	case printers[prog]:
		return true
	case prog == "openssl" && len(args) > 0:
		switch args[0] {
		case "rsa", "pkey", "ec", "pkcs12", "pkcs8", "enc", "base64":
			return !hasFlag(args, "-noout")
		}
	}
	return false
}

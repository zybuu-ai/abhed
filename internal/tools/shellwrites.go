package tools

import (
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// maxWrites bounds the files one command records as written.
const maxWrites = 64

// wrote records a file the command writes; past maxWrites the rest are not known.
func (f *shellFacts) wrote(name string) {
	if len(f.writes) >= maxWrites {
		f.writeHidden = "more files written than the check keeps"
		return
	}
	f.writes = append(f.writes, name)
}

// placeAndWrites notes what a command does to the folder later commands run
// in, and the files it writes, so a script is not read from the wrong
// folder or before the line replaces it.
func (f *shellFacts) placeAndWrites(name string, args []*syntax.Word) {
	switch name {
	case "cd", "pushd", "popd":
		f.moves = true
	case "env":
		f.moves = f.moves || optionTaken(args, "C", "--chdir")
	case "sudo":
		f.moves = f.moves || optionTaken(args, "D", "--chdir")
	case "find":
		for _, a := range args {
			if v, _ := literal(a); v == "-execdir" || v == "-okdir" {
				f.moves = true
			}
		}
	case "cp", "mv", "ln", "install", "rsync", "gcp", "gmv", "gln", "ginstall":
		f.copies(name, args)
	case "tee":
		f.operandsWritten(name, args, nil)
	case "dd":
		for _, a := range args {
			v, lit := literal(a)
			switch {
			case lit && strings.HasPrefix(v, "of="):
				f.wrote(strings.TrimPrefix(v, "of="))
			case !lit && strings.HasPrefix(wordText(a), "of="):
				f.writeHidden = "dd writing a file named by an expansion"
			}
		}
	case "sed", "gsed", "perl":
		if optionTaken(args, "i", "--in-place") {
			f.operandsWritten(name, args, map[string]bool{"-e": true, "-f": true, "--expression": true, "--file": true})
		}
	case "curl":
		f.namedOutput(name, args, "o", "--output", "O", "--remote-name")
	case "wget":
		f.namedOutput(name, args, "O", "--output-document", "", "")
		if !optionTaken(args, "O", "--output-document") {
			f.writeHidden = "wget saving under a name it takes from the address"
		}
	}
}

// optionTaken reports a short option letter, alone or in a cluster, or a long
// option, among a command's options; one built from an expansion may be either.
func optionTaken(args []*syntax.Word, short, long string) bool {
	for _, a := range args {
		v, lit := literal(a)
		if !lit {
			if t := strings.TrimLeft(wordText(a), `"'`); strings.HasPrefix(t, "-") || strings.HasPrefix(t, "$") {
				return true
			}
			continue
		}
		switch {
		case v == "--":
			return false
		case long != "" && (v == long || strings.HasPrefix(v, long+"=")):
			return true
		case short != "" && shortCluster(v) && strings.Contains(v, short):
			return true
		}
	}
	return false
}

// copies records what cp, mv, ln, install and rsync write: the target, and
// each source's name inside it, as when the target is a folder.
func (f *shellFacts) copies(name string, args []*syntax.Word) {
	var operands []string
	target, hidden, ended := "", false, false
	for j := 0; j < len(args); j++ {
		v, lit := literal(args[j])
		if !lit {
			hidden = true
			continue
		}
		switch {
		case ended || !strings.HasPrefix(v, "-") || v == "-":
			// A pattern names files the check cannot list.
			hidden = hidden || globWord(args[j])
			operands = append(operands, v)
		case v == "--":
			ended = true
		case v == "--target-directory":
			if j+1 < len(args) {
				j++
				if t, lit := literal(args[j]); lit {
					target = t
				} else {
					hidden = true
				}
			}
		case strings.HasPrefix(v, "--target-directory="):
			target = strings.TrimPrefix(v, "--target-directory=")
		case name != "rsync" && shortCluster(v) && strings.Contains(v, "t"):
			t, next := clusterValue(args, j, 't')
			if j = next; t == "" {
				hidden = true
			}
			target = t
		case (v == "-S" && name != "rsync") || (name == "install" && (v == "-m" || v == "-o" || v == "-g")) || (name == "rsync" && v == "-e"):
			j++ // the option's value
		}
	}
	if hidden {
		f.writeHidden = name + " writing a file named by an expansion or a pattern"
	}
	if target == "" && len(operands) > 0 {
		target, operands = operands[len(operands)-1], operands[:len(operands)-1]
	}
	if target == "" {
		return
	}
	f.wrote(target)
	for _, src := range operands {
		f.wrote(path.Join(target, path.Base(strings.TrimRight(src, "/"))))
	}
}

// operandsWritten records each operand of a command that writes them all,
// skipping the values of the options in valued.
func (f *shellFacts) operandsWritten(name string, args []*syntax.Word, valued map[string]bool) {
	ended := false
	for j := 0; j < len(args); j++ {
		v, lit := literal(args[j])
		switch {
		case !lit:
			f.writeHidden = name + " writing a file named by an expansion"
		case ended || !strings.HasPrefix(v, "-") || v == "-":
			f.wrote(v)
		case v == "--":
			ended = true
		case valued[v]:
			j++
		}
	}
}

// namedOutput records the file curl -o or wget -O writes; an output named
// from the address, as curl -O does, is not known.
func (f *shellFacts) namedOutput(name string, args []*syntax.Word, short, long, remote, remoteLong string) {
	for j := 0; j < len(args); j++ {
		v, lit := literal(args[j])
		switch {
		case !lit:
			if strings.HasPrefix(wordText(args[j]), "-") {
				f.writeHidden = name + " writing a file named by an expansion"
			}
		case v == "--":
			return
		case v == long:
			if j+1 < len(args) {
				j++
				if t, lit := literal(args[j]); lit {
					f.wrote(t)
				} else {
					f.writeHidden = name + " writing a file named by an expansion"
				}
			}
		case strings.HasPrefix(v, long+"="):
			f.wrote(strings.TrimPrefix(v, long+"="))
		case remote != "" && (v == remoteLong || (shortCluster(v) && strings.Contains(v, remote))):
			f.writeHidden = name + " saving under a name it takes from the address"
			if shortCluster(v) && strings.Contains(v, short) {
				_, j = clusterValue(args, j, short[0])
			}
		case shortCluster(v) && strings.Contains(v, short):
			t, next := clusterValue(args, j, short[0])
			if j = next; t == "" {
				f.writeHidden = name + " writing a file named by an expansion"
			} else {
				f.wrote(t)
			}
		}
	}
}

// shortCluster reports a word of short options: -x or -xyz.
func shortCluster(v string) bool { return len(v) > 1 && v[0] == '-' && v[1] != '-' }

// clusterValue is the value of letter in the short cluster args[j]: the rest
// of the word, or the next word when it ends the cluster. It returns "" for a
// value built from an expansion or missing, and the index of the last word used.
func clusterValue(args []*syntax.Word, j int, letter byte) (string, int) {
	v, _ := literal(args[j])
	if rest := v[strings.IndexByte(v, letter)+1:]; rest != "" {
		return rest, j
	}
	if j+1 >= len(args) {
		return "", j
	}
	t, _ := literal(args[j+1])
	return t, j + 1
}

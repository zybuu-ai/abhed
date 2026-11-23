package k8s

import (
	"fmt"
	"strconv"
	"strings"
)

// A small YAML reader for kubeconfig files.
//
// Abhed has no YAML dependency and this is not a reason to add one: a general
// YAML library is a large, historically CVE-prone surface, and an air-gapped
// bundle has to justify every dependency in it. Kubeconfig is a narrow, well
// known shape — nested maps, lists of maps, scalar strings and bools — and
// that subset is what this parses.
//
// What it deliberately does NOT support: anchors and aliases, multi-document
// files, flow style ({a: b}), block scalars (| and >), and tags. A kubeconfig
// using those is rejected with a message saying so, which is far better than
// silently misreading a cluster's CA certificate.

type yamlNode struct {
	scalar   string
	mapping  map[string]*yamlNode
	sequence []*yamlNode
	isMap    bool
	isSeq    bool
}

func parseKubeconfig(raw []byte) (*kubeconfig, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	for _, unsupported := range []struct{ token, why string }{
		{"\n---", "multi-document files"},
		{"<<:", "merge keys"},
	} {
		if strings.Contains(text, unsupported.token) {
			return nil, fmt.Errorf("this kubeconfig uses %s, which Abhed's reader "+
				"does not support; point it at a plain kubeconfig with -kubeconfig",
				unsupported.why)
		}
	}

	lines := splitLines(text)
	root, _, err := parseBlock(lines, 0, 0)
	if err != nil {
		return nil, err
	}

	kc := &kubeconfig{}
	kc.CurrentContext = root.str("current-context")

	for _, n := range root.seq("contexts") {
		e := kubeContext{Name: n.str("name")}
		if c := n.get("context"); c != nil {
			e.Cluster = c.str("cluster")
			e.User = c.str("user")
			e.Namespace = c.str("namespace")
		}
		kc.Contexts = append(kc.Contexts, e)
	}

	for _, n := range root.seq("clusters") {
		e := kubeCluster{Name: n.str("name")}
		if c := n.get("cluster"); c != nil {
			e.Server = c.str("server")
			e.CertificateAuthorityData = c.str("certificate-authority-data")
			e.CertificateAuthority = c.str("certificate-authority")
			e.InsecureSkipTLSVerify = c.boolean("insecure-skip-tls-verify")
		}
		kc.Clusters = append(kc.Clusters, e)
	}

	for _, n := range root.seq("users") {
		e := kubeUser{Name: n.str("name")}
		if u := n.get("user"); u != nil {
			e.Token = u.str("token")
			e.TokenFile = u.str("tokenFile")
			e.ClientCertificateData = u.str("client-certificate-data")
			e.ClientKeyData = u.str("client-key-data")
			e.ClientCertificate = u.str("client-certificate")
			e.ClientKey = u.str("client-key")
			if ap := u.get("auth-provider"); ap != nil {
				e.AuthProvider = ap.str("name")
				if e.AuthProvider == "" {
					e.AuthProvider = "(unnamed)"
				}
			}
			e.Username = u.str("username")
			if ex := u.get("exec"); ex != nil {
				cfg := &execConfig{Command: ex.str("command"), APIVersion: ex.str("apiVersion")}
				for _, a := range ex.seq("args") {
					cfg.Args = append(cfg.Args, a.scalar)
				}
				for _, envNode := range ex.seq("env") {
					cfg.Env = append(cfg.Env, execEnv{
						Name: envNode.str("name"), Value: envNode.str("value")})
				}
				e.Exec = cfg
			}
		}
		kc.Users = append(kc.Users, e)
	}

	return kc, nil
}

type line struct {
	indent int
	text   string
	num    int
}

func splitLines(text string) []line {
	var out []line
	for i, raw := range strings.Split(text, "\n") {
		trimmed := strings.TrimLeft(raw, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(raw, "\t") {
			// Tabs are illegal for YAML indentation and silently change how
			// the file nests. Better to be explicit than to guess.
			continue
		}
		out = append(out, line{indent: len(raw) - len(trimmed), text: trimmed, num: i + 1})
	}
	return out
}

// parseBlock reads all lines at the given indent, returning the node and the
// index of the first line that belongs to an outer block.
func parseBlock(lines []line, i, indent int) (*yamlNode, int, error) {
	node := &yamlNode{mapping: map[string]*yamlNode{}}
	for i < len(lines) {
		l := lines[i]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, i, fmt.Errorf("line %d: unexpected indentation", l.num)
		}

		if strings.HasPrefix(l.text, "- ") || l.text == "-" {
			if node.isMap {
				break // a sequence cannot continue a mapping at the same indent
			}
			node.isSeq = true
			item, next, err := parseSeqItem(lines, i, indent)
			if err != nil {
				return nil, i, err
			}
			node.sequence = append(node.sequence, item)
			i = next
			continue
		}

		if node.isSeq {
			break // the sequence ended; this key belongs to the parent
		}
		key, rest, found := strings.Cut(l.text, ":")
		if !found {
			return nil, i, fmt.Errorf("line %d: expected key: value", l.num)
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimSpace(rest)
		node.isMap = true

		if rest != "" {
			node.mapping[key] = &yamlNode{scalar: unquote(rest)}
			i++
			continue
		}
		// A nested block. Its lines are either more indented than the key, or
		// — for a sequence — at the SAME indent, which is legal YAML and is
		// what kubectl actually writes:
		//
		//   clusters:
		//   - cluster:
		//       server: https://...
		//     name: prod
		//
		// Requiring deeper indentation here parsed every real kubeconfig as
		// having zero clusters, with no error.
		if i+1 < len(lines) {
			next := lines[i+1]
			if next.indent > indent ||
				(next.indent == indent && strings.HasPrefix(next.text, "-")) {
				child, after, err := parseBlock(lines, i+1, next.indent)
				if err != nil {
					return nil, i, err
				}
				node.mapping[key] = child
				i = after
				continue
			}
		}
		node.mapping[key] = &yamlNode{}
		i++
	}
	return node, i, nil
}

// parseSeqItem reads one "- ..." entry, which may carry its first key inline.
func parseSeqItem(lines []line, i, indent int) (*yamlNode, int, error) {
	l := lines[i]
	inline := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))

	if inline == "" {
		if i+1 < len(lines) && lines[i+1].indent > indent {
			return parseBlockAt(lines, i+1, lines[i+1].indent)
		}
		return &yamlNode{}, i + 1, nil
	}
	key, rest, found := strings.Cut(inline, ":")
	if !found {
		// A bare scalar entry, as in an args list.
		return &yamlNode{scalar: unquote(inline)}, i + 1, nil
	}

	item := &yamlNode{mapping: map[string]*yamlNode{}, isMap: true}
	key = strings.TrimSpace(key)
	rest = strings.TrimSpace(rest)
	// The inline key sits at the indent of the text after "- ".
	childIndent := indent + (len(l.text) - len(inline))

	switch {
	case rest != "":
		item.mapping[key] = &yamlNode{scalar: unquote(rest)}
		i++
	case i+1 < len(lines) && lines[i+1].indent > childIndent:
		child, next, err := parseBlockAt(lines, i+1, lines[i+1].indent)
		if err != nil {
			return nil, i, err
		}
		item.mapping[key] = child
		i = next
	default:
		item.mapping[key] = &yamlNode{}
		i++
	}

	// Remaining keys of this item sit at childIndent.
	for i < len(lines) && lines[i].indent == childIndent &&
		!strings.HasPrefix(lines[i].text, "- ") {
		k, rest, found := strings.Cut(lines[i].text, ":")
		if !found {
			return nil, i, fmt.Errorf("line %d: expected key: value", lines[i].num)
		}
		k = strings.TrimSpace(k)
		rest = strings.TrimSpace(rest)
		if rest != "" {
			item.mapping[k] = &yamlNode{scalar: unquote(rest)}
			i++
			continue
		}
		if i+1 < len(lines) && lines[i+1].indent > childIndent {
			child, next, err := parseBlockAt(lines, i+1, lines[i+1].indent)
			if err != nil {
				return nil, i, err
			}
			item.mapping[k] = child
			i = next
			continue
		}
		item.mapping[k] = &yamlNode{}
		i++
	}
	return item, i, nil
}

func parseBlockAt(lines []line, i, indent int) (*yamlNode, int, error) {
	return parseBlock(lines, i, indent)
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			inner := s[1 : len(s)-1]
			if s[0] == '"' {
				if u, err := strconv.Unquote(s); err == nil {
					return u
				}
			}
			return inner
		}
	}
	return s
}

func (n *yamlNode) get(key string) *yamlNode {
	if n == nil || n.mapping == nil {
		return nil
	}
	return n.mapping[key]
}

func (n *yamlNode) str(key string) string {
	c := n.get(key)
	if c == nil {
		return ""
	}
	return c.scalar
}

func (n *yamlNode) boolean(key string) bool {
	v := strings.ToLower(n.str(key))
	return v == "true" || v == "yes"
}

func (n *yamlNode) seq(key string) []*yamlNode {
	c := n.get(key)
	if c == nil {
		return nil
	}
	return c.sequence
}

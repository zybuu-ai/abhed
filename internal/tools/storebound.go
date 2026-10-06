package tools

import "github.com/zybuu-ai/abhed/internal/secrets"

// StoreBound is a tool that reads the secrets store. A server that keeps one
// store per account binds each session's copy to the store of its owner.
type StoreBound interface {
	Tool
	BindStore(v *secrets.Store) Tool
}

// BindStore is bash reading v, and offering v's names, in place of its store.
func (b Bash) BindStore(v *secrets.Store) Tool {
	b.Secrets, b.SecretNames = v.Env, v.Offered()
	return b
}

// BindStores is a copy of r whose every StoreBound tool reads v instead.
func BindStores(r *Registry, v *secrets.Store) *Registry {
	out := r.Clone()
	for _, t := range r.All() {
		if sb, ok := t.(StoreBound); ok {
			out.Add(sb.BindStore(v))
		}
	}
	return out
}

// Package env is the single declaration point for HASteward's environment-variable
// bindings. Each helper does three things at once — creates the Cobra flag, seeds its
// default from the environment (via src/common), and registers the flag↔variable link
// — so a binding is declared in exactly one place.
//
// The registry deliberately owns ONLY what Cobra cannot know: the environment variable
// name and whether it is prefixed or raw. Type, default, description, and shorthand stay
// owned by the Cobra flag and are read back at doc-gen time (see internal/docsgen). That
// keeps the generated Environment Variables reference free of any duplicated metadata,
// so it can never drift from the flags it documents.
package env

import (
	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/spf13/pflag"
)

// Prefix is the environment-variable prefix for HASteward's own settings.
const Prefix = common.EnvPrefix // "HASTEWARD_"

// Binding is the one fact Cobra can't recover: the environment variable that backs a
// persistent flag. Exactly one of Key (prefixed) or Raw (unprefixed) is set.
type Binding struct {
	Flag string // persistent flag name, e.g. "engine"
	Key  string // HASTEWARD_<Key>, e.g. "ENGINE"; empty when Raw is set
	Raw  string // unprefixed variable, e.g. "KUBECONFIG"; empty when Key is set
}

// Name returns the full environment-variable name (prefixed or raw).
func (b Binding) Name() string {
	if b.Raw != "" {
		return b.Raw
	}
	return Prefix + b.Key
}

var bindings []Binding

// Bindings returns the registered env↔flag bindings in declaration order.
func Bindings() []Binding { return bindings }

func register(b Binding) { bindings = append(bindings, b) }

// String binds --<flag> to *p, defaulting to HASTEWARD_<key> (else def), and registers it.
func String(pf *pflag.FlagSet, p *string, flag, shorthand, key, def, usage string) {
	register(Binding{Flag: flag, Key: key})
	pf.StringVarP(p, flag, shorthand, common.Env(key, def), usage)
}

// Bool binds a bool --<flag> defaulting to HASTEWARD_<key> (else def).
func Bool(pf *pflag.FlagSet, p *bool, flag, shorthand, key string, def bool, usage string) {
	register(Binding{Flag: flag, Key: key})
	pf.BoolVarP(p, flag, shorthand, common.EnvBool(key, def), usage)
}

// Int binds an int --<flag> defaulting to HASTEWARD_<key> (else def).
func Int(pf *pflag.FlagSet, p *int, flag, shorthand, key string, def int, usage string) {
	register(Binding{Flag: flag, Key: key})
	pf.IntVarP(p, flag, shorthand, common.EnvInt(key, def), usage)
}

// Raw binds --<flag> to an UNPREFIXED variable (e.g. KUBECONFIG), defaulting to it (else def).
func Raw(pf *pflag.FlagSet, p *string, flag, shorthand, rawKey, def, usage string) {
	register(Binding{Flag: flag, Raw: rawKey})
	pf.StringVarP(p, flag, shorthand, common.EnvRaw(rawKey, def), usage)
}

// RawOrPrefixed reads the unprefixed variable first, then HASTEWARD_<rawKey> as a
// fallback. It documents the unprefixed name — the canonical one (e.g. RESTIC_PASSWORD).
func RawOrPrefixed(pf *pflag.FlagSet, p *string, flag, shorthand, rawKey, def, usage string) {
	register(Binding{Flag: flag, Raw: rawKey})
	pf.StringVarP(p, flag, shorthand, common.EnvRaw(rawKey, common.Env(rawKey, def)), usage)
}

// StringP registers a HASTEWARD_<key> binding for a flag with no bound target (read
// later via cmd.Flags().GetString). Mirrors pflag.StringP.
func StringP(pf *pflag.FlagSet, flag, shorthand, key, def, usage string) {
	register(Binding{Flag: flag, Key: key})
	pf.StringP(flag, shorthand, common.Env(key, def), usage)
}

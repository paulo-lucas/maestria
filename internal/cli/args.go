package cli

import (
	"fmt"
	"strings"
)

// flagSpec declares expected CLI flags for flexible V1-compatible parsing anywhere in argv.
type flagSpec struct {
	Bools   map[string]bool
	Strings map[string]bool
	Multi   map[string]bool
}

type parsedArgs struct {
	Bools   map[string]bool
	Strings map[string]string
	Multi   map[string][]string
	Pos     []string
}

func (p *parsedArgs) Bool(name string) bool       { return p.Bools[name] }
func (p *parsedArgs) HasString(name string) bool  { _, ok := p.Strings[name]; return ok }
func (p *parsedArgs) String(name string) string   { return p.Strings[name] }
func (p *parsedArgs) MultiVal(name string) []string { return p.Multi[name] }

func parseFlexArgs(args []string, spec flagSpec) (*parsedArgs, error) {
	out := &parsedArgs{
		Bools:   map[string]bool{},
		Strings: map[string]string{},
		Multi:   map[string][]string{},
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out.Pos = append(out.Pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			out.Pos = append(out.Pos, arg)
			continue
		}
		name := arg[2:]
		hasEq := false
		eqVal := ""
		if idx := strings.IndexByte(name, '='); idx >= 0 {
			eqVal = name[idx+1:]
			name = name[:idx]
			hasEq = true
		}

		switch {
		case spec.Bools[name]:
			if hasEq {
				switch strings.ToLower(eqVal) {
				case "1", "true", "yes":
					out.Bools[name] = true
				case "0", "false", "no":
					out.Bools[name] = false
				default:
					return nil, fmt.Errorf("invalid boolean value %q for --%s", eqVal, name)
				}
			} else {
				out.Bools[name] = true
			}
		case spec.Strings[name] || spec.Multi[name]:
			var val string
			if hasEq {
				val = eqVal
			} else {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("flag --%s requires a value", name)
				}
				i++
				val = args[i]
			}
			if spec.Multi[name] {
				out.Multi[name] = append(out.Multi[name], val)
			} else {
				out.Strings[name] = val
			}
		default:
			return nil, fmt.Errorf("unknown flag: --%s", name)
		}
	}
	return out, nil
}

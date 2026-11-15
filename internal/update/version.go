package update

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a semantic version (https://semver.org).
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string
	Build               string
}

// ParseVersion parses "1.2.3", "v1.2.3", "1.2.3-rc.1" and "1.2.3-rc.1+meta".
func ParseVersion(s string) (Version, error) {
	orig := s
	s = CleanVersion(s)
	var v Version
	if i := strings.IndexByte(s, '+'); i >= 0 {
		v.Build = s[i+1:]
		s = s[:i]
		if v.Build == "" {
			return Version{}, fmt.Errorf("invalid version %q", orig)
		}
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.Pre = strings.Split(s[i+1:], ".")
		s = s[:i]
		for _, id := range v.Pre {
			if !validIdent(id) {
				return Version{}, fmt.Errorf("invalid version %q", orig)
			}
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid version %q", orig)
	}
	nums := [3]*uint64{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("invalid version %q", orig)
		}
		n, err := strconv.ParseUint(p, 10, 63)
		if err != nil {
			return Version{}, fmt.Errorf("invalid version %q", orig)
		}
		*nums[i] = n
	}
	return v, nil
}

func validIdent(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
			return false
		}
	}
	if isNumeric(id) && len(id) > 1 && id[0] == '0' {
		return false
	}
	return true
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// String formats the version without a leading "v".
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// IsPrerelease reports whether the version has a pre-release part.
func (v Version) IsPrerelease() bool { return len(v.Pre) > 0 }

// Compare returns -1, 0 or 1 by semver precedence (build metadata is ignored).
func (v Version) Compare(o Version) int {
	for _, p := range [][2]uint64{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		a, b := v.Pre[i], o.Pre[i]
		if a == b {
			continue
		}
		an, bn := isNumeric(a), isNumeric(b)
		switch {
		case an && bn:
			x, _ := strconv.ParseUint(a, 10, 64)
			y, _ := strconv.ParseUint(b, 10, 64)
			if x < y {
				return -1
			}
			return 1
		case an:
			return -1
		case bn:
			return 1
		case a < b:
			return -1
		default:
			return 1
		}
	}
	switch {
	case len(v.Pre) < len(o.Pre):
		return -1
	case len(v.Pre) > len(o.Pre):
		return 1
	}
	return 0
}

// IsDevBuild reports whether a binary's version is a development build that must never update
// itself: unset, "dev", not semver, carrying build metadata ("2.0.0-dev+abc1234"), or a "-dev"
// pre-release. Release builds are plain semver without metadata.
func IsDevBuild(version string) bool {
	v, err := ParseVersion(version)
	if err != nil {
		return true
	}
	if v.Build != "" {
		return true
	}
	return len(v.Pre) > 0 && v.Pre[0] == "dev"
}

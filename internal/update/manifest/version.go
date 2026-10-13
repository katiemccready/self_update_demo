package manifest

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

type Version struct{ Major, Minor, Patch int }

func ParseVersion(s string) (Version, error) {
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid version %q: want MAJOR.MINOR.PATCH", s)
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)

		if err != nil || v < 0 || strconv.Itoa(v) != p {
			return Version{}, fmt.Errorf("invalid version %q: bad component %q", s, p)
		}
		n[i] = v
	}
	return Version{n[0], n[1], n[2]}, nil
}

func (v Version) Compare(o Version) int {
	return cmp.Or(
		cmp.Compare(v.Major, o.Major),
		cmp.Compare(v.Minor, o.Minor),
		cmp.Compare(v.Patch, o.Patch),
	)
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }
